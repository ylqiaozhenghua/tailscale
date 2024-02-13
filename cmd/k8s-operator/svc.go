// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

//go:build !plan9

package main

import (
	"context"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"sync"

	"go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	tsoperator "tailscale.com/k8s-operator"
	tsapi "tailscale.com/k8s-operator/apis/v1alpha1"
	"tailscale.com/util/clientmetric"
	"tailscale.com/util/set"
)

type ServiceReconciler struct {
	client.Client
	ssr                   *tailscaleSTSReconciler
	logger                *zap.SugaredLogger
	isDefaultLoadBalancer bool

	mu sync.Mutex // protects following

	// managedIngressProxies is a set of all ingress proxies that we're
	// currently managing. This is only used for metrics.
	managedIngressProxies set.Slice[types.UID]
	// managedEgressProxies is a set of all egress proxies that we're currently
	// managing. This is only used for metrics.
	managedEgressProxies set.Slice[types.UID]

	recorder record.EventRecorder
}

var (
	// gaugeEgressProxies tracks the number of egress proxies that we're
	// currently managing.
	gaugeEgressProxies = clientmetric.NewGauge("k8s_egress_proxies")
	// gaugeIngressProxies tracks the number of ingress proxies that we're
	// currently managing.
	gaugeIngressProxies = clientmetric.NewGauge("k8s_ingress_proxies")
)

func childResourceLabels(name, ns, typ string) map[string]string {
	// You might wonder why we're using owner references, since they seem to be
	// built for exactly this. Unfortunately, Kubernetes does not support
	// cross-namespace ownership, by design. This means we cannot make the
	// service being exposed the owner of the implementation details of the
	// proxying. Instead, we have to do our own filtering and tracking with
	// labels.
	return map[string]string{
		LabelManaged:         "true",
		LabelParentName:      name,
		LabelParentNamespace: ns,
		LabelParentType:      typ,
	}
}

func (a *ServiceReconciler) Reconcile(ctx context.Context, req reconcile.Request) (_ reconcile.Result, err error) {
	logger := a.logger.With("service-ns", req.Namespace, "service-name", req.Name)
	logger.Debugf("starting reconcile")
	defer logger.Debugf("reconcile finished")

	svc := new(corev1.Service)
	err = a.Get(ctx, req.NamespacedName, svc)
	if apierrors.IsNotFound(err) {
		// Request object not found, could have been deleted after reconcile request.
		logger.Debugf("service not found, assuming it was deleted")
		return reconcile.Result{}, nil
	} else if err != nil {
		return reconcile.Result{}, fmt.Errorf("failed to get svc: %w", err)
	}
	targetIP := a.tailnetTargetAnnotation(svc)
	targetFQDN := svc.Annotations[AnnotationTailnetTargetFQDN]
	if !svc.DeletionTimestamp.IsZero() || !a.shouldExpose(svc) && targetIP == "" && targetFQDN == "" {
		logger.Debugf("service is being deleted or is (no longer) referring to Tailscale ingress/egress, ensuring any created resources are cleaned up")
		return reconcile.Result{}, a.maybeCleanup(ctx, logger, svc)
	}

	return reconcile.Result{}, a.maybeProvision(ctx, logger, svc)
}

// maybeCleanup removes any existing resources related to serving svc over tailscale.
//
// This function is responsible for removing the finalizer from the service,
// once all associated resources are gone.
func (a *ServiceReconciler) maybeCleanup(ctx context.Context, logger *zap.SugaredLogger, svc *corev1.Service) error {
	ix := slices.Index(svc.Finalizers, FinalizerName)
	if ix < 0 {
		logger.Debugf("no finalizer, nothing to do")
		a.mu.Lock()
		defer a.mu.Unlock()
		a.managedIngressProxies.Remove(svc.UID)
		a.managedEgressProxies.Remove(svc.UID)
		gaugeIngressProxies.Set(int64(a.managedIngressProxies.Len()))
		gaugeEgressProxies.Set(int64(a.managedEgressProxies.Len()))
		return nil
	}

	if done, err := a.ssr.Cleanup(ctx, logger, childResourceLabels(svc.Name, svc.Namespace, "svc")); err != nil {
		return fmt.Errorf("failed to cleanup: %w", err)
	} else if !done {
		logger.Debugf("cleanup not done yet, waiting for next reconcile")
		return nil
	}

	svc.Finalizers = append(svc.Finalizers[:ix], svc.Finalizers[ix+1:]...)
	if err := a.Update(ctx, svc); err != nil {
		return fmt.Errorf("failed to remove finalizer: %w", err)
	}

	// Unlike most log entries in the reconcile loop, this will get printed
	// exactly once at the very end of cleanup, because the final step of
	// cleanup removes the tailscale finalizer, which will make all future
	// reconciles exit early.
	logger.Infof("unexposed service from tailnet")

	a.mu.Lock()
	defer a.mu.Unlock()
	a.managedIngressProxies.Remove(svc.UID)
	a.managedEgressProxies.Remove(svc.UID)
	gaugeIngressProxies.Set(int64(a.managedIngressProxies.Len()))
	gaugeEgressProxies.Set(int64(a.managedEgressProxies.Len()))
	return nil
}

// maybeProvision ensures that svc is exposed over tailscale, taking any actions
// necessary to reach that state.
//
// This function adds a finalizer to svc, ensuring that we can handle orderly
// deprovisioning later.
func (a *ServiceReconciler) maybeProvision(ctx context.Context, logger *zap.SugaredLogger, svc *corev1.Service) error {
	// Run for proxy config related validations here as opposed to running
	// them earlier. This is to prevent cleanup being blocked on a
	// misconfigured proxy param.
	if err := a.ssr.validate(); err != nil {
		msg := fmt.Sprintf("unable to provision proxy resources: invalid config: %v", err)
		a.recorder.Event(svc, corev1.EventTypeWarning, "INVALIDCONFIG", msg)
		a.logger.Error(msg)
		return nil
	}
	if violations := validateService(svc); len(violations) > 0 {
		msg := fmt.Sprintf("unable to provision proxy resources: invalid Service: %s", strings.Join(violations, ", "))
		a.recorder.Event(svc, corev1.EventTypeWarning, "INVALIDSERVCICE", msg)
		a.logger.Error(msg)
		return nil
	}

	proxyClass := proxyClassForObject(svc)
	if proxyClass != "" {
		if ready, err := proxyClassIsReady(ctx, proxyClass, a.Client); err != nil {
			return fmt.Errorf("error verifying ProxyClass for Service: %w", err)
		} else if !ready {
			logger.Infof("ProxyClass %s specified for the Service, but is not (yet) Ready, waiting..", proxyClass)
			return nil
		}
	}

	hostname, err := nameForService(svc)
	if err != nil {
		return err
	}

	if !slices.Contains(svc.Finalizers, FinalizerName) {
		// This log line is printed exactly once during initial provisioning,
		// because once the finalizer is in place this block gets skipped. So,
		// this is a nice place to tell the operator that the high level,
		// multi-reconcile operation is underway.
		logger.Infof("exposing service over tailscale")
		svc.Finalizers = append(svc.Finalizers, FinalizerName)
		if err := a.Update(ctx, svc); err != nil {
			return fmt.Errorf("failed to add finalizer: %w", err)
		}
	}
	crl := childResourceLabels(svc.Name, svc.Namespace, "svc")
	var tags []string
	if tstr, ok := svc.Annotations[AnnotationTags]; ok {
		tags = strings.Split(tstr, ",")
	}

	sts := &tailscaleSTSConfig{
		ParentResourceName:  svc.Name,
		ParentResourceUID:   string(svc.UID),
		Hostname:            hostname,
		Tags:                tags,
		ChildResourceLabels: crl,
		ProxyClass:          proxyClass,
	}

	a.mu.Lock()
	if a.shouldExpose(svc) {
		sts.ClusterTargetIP = svc.Spec.ClusterIP
		a.managedIngressProxies.Add(svc.UID)
		gaugeIngressProxies.Set(int64(a.managedIngressProxies.Len()))
	} else if ip := a.tailnetTargetAnnotation(svc); ip != "" {
		sts.TailnetTargetIP = ip
		a.managedEgressProxies.Add(svc.UID)
		gaugeEgressProxies.Set(int64(a.managedEgressProxies.Len()))
	} else if fqdn := svc.Annotations[AnnotationTailnetTargetFQDN]; fqdn != "" {
		fqdn := svc.Annotations[AnnotationTailnetTargetFQDN]
		if !strings.HasSuffix(fqdn, ".") {
			fqdn = fqdn + "."
		}
		sts.TailnetTargetFQDN = fqdn
		a.managedEgressProxies.Add(svc.UID)
		gaugeEgressProxies.Set(int64(a.managedEgressProxies.Len()))
	}
	a.mu.Unlock()

	var hsvc *corev1.Service
	if hsvc, err = a.ssr.Provision(ctx, logger, sts); err != nil {
		return fmt.Errorf("failed to provision: %w", err)
	}

	if sts.TailnetTargetIP != "" || sts.TailnetTargetFQDN != "" {
		// TODO (irbekrm): cluster.local is the default DNS name, but
		// can be changed by users. Make this configurable or figure out
		// how to discover the DNS name from within operator
		headlessSvcName := hsvc.Name + "." + hsvc.Namespace + ".svc.cluster.local"
		if svc.Spec.ExternalName != headlessSvcName || svc.Spec.Type != corev1.ServiceTypeExternalName {
			svc.Spec.ExternalName = headlessSvcName
			svc.Spec.Selector = nil
			svc.Spec.Type = corev1.ServiceTypeExternalName
			if err := a.Update(ctx, svc); err != nil {
				return fmt.Errorf("failed to update service: %w", err)
			}
		}
		return nil
	}

	if !a.hasLoadBalancerClass(svc) {
		logger.Debugf("service is not a LoadBalancer, so not updating ingress")
		return nil
	}

	_, tsHost, tsIPs, err := a.ssr.DeviceInfo(ctx, crl)
	if err != nil {
		return fmt.Errorf("failed to get device ID: %w", err)
	}
	if tsHost == "" {
		logger.Debugf("no Tailscale hostname known yet, waiting for proxy pod to finish auth")
		// No hostname yet. Wait for the proxy pod to auth.
		svc.Status.LoadBalancer.Ingress = nil
		if err := a.Status().Update(ctx, svc); err != nil {
			return fmt.Errorf("failed to update service status: %w", err)
		}
		return nil
	}

	logger.Debugf("setting ingress to %q, %s", tsHost, strings.Join(tsIPs, ", "))
	ingress := []corev1.LoadBalancerIngress{
		{Hostname: tsHost},
	}
	clusterIPAddr, err := netip.ParseAddr(svc.Spec.ClusterIP)
	if err != nil {
		return fmt.Errorf("failed to parse cluster IP: %w", err)
	}
	for _, ip := range tsIPs {
		addr, err := netip.ParseAddr(ip)
		if err != nil {
			continue
		}
		if addr.Is4() == clusterIPAddr.Is4() { // only add addresses of the same family
			ingress = append(ingress, corev1.LoadBalancerIngress{IP: ip})
		}
	}
	svc.Status.LoadBalancer.Ingress = ingress
	if err := a.Status().Update(ctx, svc); err != nil {
		return fmt.Errorf("failed to update service status: %w", err)
	}
	return nil
}

func validateService(svc *corev1.Service) []string {
	violations := make([]string, 0)
	if svc.Annotations[AnnotationTailnetTargetFQDN] != "" && svc.Annotations[AnnotationTailnetTargetIP] != "" {
		violations = append(violations, "only one of annotations %s and %s can be set", AnnotationTailnetTargetIP, AnnotationTailnetTargetFQDN)
	}
	if fqdn := svc.Annotations[AnnotationTailnetTargetFQDN]; fqdn != "" {
		if !isMagicDNSName(fqdn) {
			violations = append(violations, fmt.Sprintf("invalid value of annotation %s: %q does not appear to be a valid MagicDNS name", AnnotationTailnetTargetFQDN, fqdn))
		}
	}
	return violations
}

func (a *ServiceReconciler) shouldExpose(svc *corev1.Service) bool {
	// Headless services can't be exposed, since there is no ClusterIP to
	// forward to.
	if svc.Spec.ClusterIP == "" || svc.Spec.ClusterIP == "None" {
		return false
	}

	return a.hasLoadBalancerClass(svc) || a.hasExposeAnnotation(svc)
}

func (a *ServiceReconciler) hasLoadBalancerClass(svc *corev1.Service) bool {
	return svc != nil &&
		svc.Spec.Type == corev1.ServiceTypeLoadBalancer &&
		(svc.Spec.LoadBalancerClass != nil && *svc.Spec.LoadBalancerClass == "tailscale" ||
			svc.Spec.LoadBalancerClass == nil && a.isDefaultLoadBalancer)
}

// hasExposeAnnotation reports whether Service has the tailscale.com/expose
// annotation set
func (a *ServiceReconciler) hasExposeAnnotation(svc *corev1.Service) bool {
	return svc != nil && svc.Annotations[AnnotationExpose] == "true"
}

// hasTailnetTargetAnnotation returns the value of tailscale.com/tailnet-ip
// annotation or of the deprecated tailscale.com/ts-tailnet-target-ip
// annotation. If neither is set, it returns an empty string. If both are set,
// it returns the value of the new annotation.
func (a *ServiceReconciler) tailnetTargetAnnotation(svc *corev1.Service) string {
	if svc == nil {
		return ""
	}
	if ip := svc.Annotations[AnnotationTailnetTargetIP]; ip != "" {
		return ip
	}
	return svc.Annotations[annotationTailnetTargetIPOld]
}

func proxyClassForObject(o client.Object) string {
	return o.GetLabels()[LabelProxyClass]
}

func proxyClassIsReady(ctx context.Context, name string, cl client.Client) (bool, error) {
	proxyClass := new(tsapi.ProxyClass)
	if err := cl.Get(ctx, types.NamespacedName{Name: name}, proxyClass); err != nil {
		return false, fmt.Errorf("error getting ProxyClass %s: %w", name, err)
	}
	return tsoperator.ProxyClassIsReady(proxyClass), nil
}
