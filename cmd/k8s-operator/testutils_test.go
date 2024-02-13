// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

//go:build !plan9

package main

import (
	"context"
	"encoding/json"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"tailscale.com/client/tailscale"
	"tailscale.com/ipn"
	tsapi "tailscale.com/k8s-operator/apis/v1alpha1"
	"tailscale.com/types/ptr"
	"tailscale.com/util/mak"
)

// confgOpts contains configuration options for creating cluster resources for
// Tailscale proxies.
type configOpts struct {
	stsName                                        string
	secretName                                     string
	hostname                                       string
	namespace                                      string
	parentType                                     string
	priorityClassName                              string
	firewallMode                                   string
	tailnetTargetIP                                string
	tailnetTargetFQDN                              string
	clusterTargetIP                                string
	subnetRoutes                                   string
	isExitNode                                     bool
	shouldUseDeclarativeConfig                     bool // tailscaled in proxy should be configured using config file
	confFileHash                                   string
	serveConfig                                    *ipn.ServeConfig
	shouldEnableForwardingClusterTrafficViaIngress bool
	proxyClass                                     string // configuration from the named ProxyClass should be applied to proxy resources
}

func expectedSTS(t *testing.T, cl client.Client, opts configOpts) *appsv1.StatefulSet {
	t.Helper()
	tsContainer := corev1.Container{
		Name:  "tailscale",
		Image: "tailscale/tailscale",
		Env: []corev1.EnvVar{
			{Name: "TS_USERSPACE", Value: "false"},
			{Name: "TS_AUTH_ONCE", Value: "true"},
			{Name: "POD_IP", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{APIVersion: "", FieldPath: "status.podIP"}, ResourceFieldRef: nil, ConfigMapKeyRef: nil, SecretKeyRef: nil}},
			{Name: "TS_KUBE_SECRET", Value: opts.secretName},
		},
		SecurityContext: &corev1.SecurityContext{
			Capabilities: &corev1.Capabilities{
				Add: []corev1.Capability{"NET_ADMIN"},
			},
		},
		ImagePullPolicy: "Always",
	}
	if opts.shouldEnableForwardingClusterTrafficViaIngress {
		tsContainer.Env = append(tsContainer.Env, corev1.EnvVar{
			Name:  "EXPERIMENTAL_ALLOW_PROXYING_CLUSTER_TRAFFIC_VIA_INGRESS",
			Value: "true",
		})
	}
	annots := make(map[string]string)
	var volumes []corev1.Volume
	if opts.shouldUseDeclarativeConfig {
		volumes = []corev1.Volume{
			{
				Name: "tailscaledconfig",
				VolumeSource: corev1.VolumeSource{
					Secret: &corev1.SecretVolumeSource{
						SecretName: opts.secretName,
						Items: []corev1.KeyToPath{
							{
								Key:  "tailscaled",
								Path: "tailscaled",
							},
						},
					},
				},
			},
		}
		tsContainer.VolumeMounts = []corev1.VolumeMount{{
			Name:      "tailscaledconfig",
			ReadOnly:  true,
			MountPath: "/etc/tsconfig",
		}}
		tsContainer.Env = append(tsContainer.Env, corev1.EnvVar{
			Name:  "EXPERIMENTAL_TS_CONFIGFILE_PATH",
			Value: "/etc/tsconfig/tailscaled",
		})
		annots["tailscale.com/operator-last-set-config-file-hash"] = opts.confFileHash
	} else {
		tsContainer.Env = append(tsContainer.Env, corev1.EnvVar{Name: "TS_HOSTNAME", Value: opts.hostname})
		annots["tailscale.com/operator-last-set-hostname"] = opts.hostname
	}
	if opts.firewallMode != "" {
		tsContainer.Env = append(tsContainer.Env, corev1.EnvVar{
			Name:  "TS_DEBUG_FIREWALL_MODE",
			Value: opts.firewallMode,
		})
	}
	if opts.tailnetTargetIP != "" {
		annots["tailscale.com/operator-last-set-ts-tailnet-target-ip"] = opts.tailnetTargetIP
		tsContainer.Env = append(tsContainer.Env, corev1.EnvVar{
			Name:  "TS_TAILNET_TARGET_IP",
			Value: opts.tailnetTargetIP,
		})
	} else if opts.tailnetTargetFQDN != "" {
		annots["tailscale.com/operator-last-set-ts-tailnet-target-fqdn"] = opts.tailnetTargetFQDN
		tsContainer.Env = append(tsContainer.Env, corev1.EnvVar{
			Name:  "TS_TAILNET_TARGET_FQDN",
			Value: opts.tailnetTargetFQDN,
		})

	} else if opts.clusterTargetIP != "" {
		tsContainer.Env = append(tsContainer.Env, corev1.EnvVar{
			Name:  "TS_DEST_IP",
			Value: opts.clusterTargetIP,
		})
		annots["tailscale.com/operator-last-set-cluster-ip"] = opts.clusterTargetIP
	}
	if opts.serveConfig != nil {
		tsContainer.Env = append(tsContainer.Env, corev1.EnvVar{
			Name:  "TS_SERVE_CONFIG",
			Value: "/etc/tailscaled/serve-config",
		})
		volumes = append(volumes, corev1.Volume{
			Name: "serve-config", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: opts.secretName, Items: []corev1.KeyToPath{{Path: "serve-config", Key: "serve-config"}}}},
		})
		tsContainer.VolumeMounts = append(tsContainer.VolumeMounts, corev1.VolumeMount{Name: "serve-config", ReadOnly: true, MountPath: "/etc/tailscaled"})
	}
	ss := &appsv1.StatefulSet{
		TypeMeta: metav1.TypeMeta{
			Kind:       "StatefulSet",
			APIVersion: "apps/v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      opts.stsName,
			Namespace: "operator-ns",
			Labels: map[string]string{
				"tailscale.com/managed":              "true",
				"tailscale.com/parent-resource":      "test",
				"tailscale.com/parent-resource-ns":   opts.namespace,
				"tailscale.com/parent-resource-type": opts.parentType,
			},
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas: ptr.To[int32](1),
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": "1234-UID"},
			},
			ServiceName: opts.stsName,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Annotations:                annots,
					DeletionGracePeriodSeconds: ptr.To[int64](10),
					Labels: map[string]string{
						"tailscale.com/managed":              "true",
						"tailscale.com/parent-resource":      "test",
						"tailscale.com/parent-resource-ns":   opts.namespace,
						"tailscale.com/parent-resource-type": opts.parentType,
						"app":                                "1234-UID",
					},
				},
				Spec: corev1.PodSpec{
					ServiceAccountName: "proxies",
					PriorityClassName:  opts.priorityClassName,
					InitContainers: []corev1.Container{
						{
							Name:    "sysctler",
							Image:   "tailscale/tailscale",
							Command: []string{"/bin/sh"},
							Args:    []string{"-c", "sysctl -w net.ipv4.ip_forward=1 net.ipv6.conf.all.forwarding=1"},
							SecurityContext: &corev1.SecurityContext{
								Privileged: ptr.To(true),
							},
						},
					},
					Containers: []corev1.Container{tsContainer},
					Volumes:    volumes,
				},
			},
		},
	}
	// If opts.proxyClass is set, retrieve the ProxyClass and apply
	// configuration from that to the StatefulSet.
	if opts.proxyClass != "" {
		t.Logf("applying configuration from ProxyClass %s", opts.proxyClass)
		proxyClass := new(tsapi.ProxyClass)
		if err := cl.Get(context.Background(), types.NamespacedName{Name: opts.proxyClass}, proxyClass); err != nil {
			t.Fatalf("error getting ProxyClass: %v", err)
		}
		return applyProxyClassToStatefulSet(proxyClass, ss)
	}
	return ss
}

func expectedSTSUserspace(t *testing.T, cl client.Client, opts configOpts) *appsv1.StatefulSet {
	tsContainer := corev1.Container{
		Name:  "tailscale",
		Image: "tailscale/tailscale",
		Env: []corev1.EnvVar{
			{Name: "TS_USERSPACE", Value: "true"},
			{Name: "TS_AUTH_ONCE", Value: "true"},
			{Name: "TS_KUBE_SECRET", Value: opts.secretName},
			{Name: "TS_HOSTNAME", Value: opts.hostname},
			{Name: "TS_SERVE_CONFIG", Value: "/etc/tailscaled/serve-config"},
		},
		ImagePullPolicy: "Always",
		VolumeMounts:    []corev1.VolumeMount{{Name: "serve-config", ReadOnly: true, MountPath: "/etc/tailscaled"}},
	}
	annots := make(map[string]string)
	volumes := []corev1.Volume{{Name: "serve-config", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: opts.secretName, Items: []corev1.KeyToPath{{Key: "serve-config", Path: "serve-config"}}}}}}
	annots["tailscale.com/operator-last-set-hostname"] = opts.hostname
	ss := &appsv1.StatefulSet{
		TypeMeta: metav1.TypeMeta{
			Kind:       "StatefulSet",
			APIVersion: "apps/v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      opts.stsName,
			Namespace: "operator-ns",
			Labels: map[string]string{
				"tailscale.com/managed":              "true",
				"tailscale.com/parent-resource":      "test",
				"tailscale.com/parent-resource-ns":   opts.namespace,
				"tailscale.com/parent-resource-type": opts.parentType,
			},
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas: ptr.To[int32](1),
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": "1234-UID"},
			},
			ServiceName: opts.stsName,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Annotations:                annots,
					DeletionGracePeriodSeconds: ptr.To[int64](10),
					Labels: map[string]string{
						"tailscale.com/managed":              "true",
						"tailscale.com/parent-resource":      "test",
						"tailscale.com/parent-resource-ns":   opts.namespace,
						"tailscale.com/parent-resource-type": opts.parentType,
						"app":                                "1234-UID",
					},
				},
				Spec: corev1.PodSpec{
					ServiceAccountName: "proxies",
					PriorityClassName:  opts.priorityClassName,
					Containers:         []corev1.Container{tsContainer},
					Volumes:            volumes,
				},
			},
		},
	}
	// If opts.proxyClass is set, retrieve the ProxyClass and apply
	// configuration from that to the StatefulSet.
	if opts.proxyClass != "" {
		t.Logf("applying configuration from ProxyClass %s", opts.proxyClass)
		proxyClass := new(tsapi.ProxyClass)
		if err := cl.Get(context.Background(), types.NamespacedName{Name: opts.proxyClass}, proxyClass); err != nil {
			t.Fatalf("error getting ProxyClass: %v", err)
		}
		return applyProxyClassToStatefulSet(proxyClass, ss)
	}
	return ss
}

func expectedHeadlessService(name string, parentType string) *corev1.Service {
	return &corev1.Service{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Service",
			APIVersion: "v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:         name,
			GenerateName: "ts-test-",
			Namespace:    "operator-ns",
			Labels: map[string]string{
				"tailscale.com/managed":              "true",
				"tailscale.com/parent-resource":      "test",
				"tailscale.com/parent-resource-ns":   "default",
				"tailscale.com/parent-resource-type": parentType,
			},
		},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{
				"app": "1234-UID",
			},
			ClusterIP: "None",
		},
	}
}

func expectedSecret(t *testing.T, opts configOpts) *corev1.Secret {
	t.Helper()
	labels := map[string]string{
		"tailscale.com/managed":              "true",
		"tailscale.com/parent-resource":      "test",
		"tailscale.com/parent-resource-type": opts.parentType,
	}
	s := &corev1.Secret{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Secret",
			APIVersion: "v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      opts.secretName,
			Namespace: "operator-ns",
		},
	}
	if opts.serveConfig != nil {
		serveConfigBs, err := json.Marshal(opts.serveConfig)
		if err != nil {
			t.Fatalf("error marshalling serve config: %v", err)
		}
		mak.Set(&s.StringData, "serve-config", string(serveConfigBs))
	}
	if !opts.shouldUseDeclarativeConfig {
		mak.Set(&s.StringData, "authkey", "secret-authkey")
		labels["tailscale.com/parent-resource-ns"] = opts.namespace
	} else {
		conf := &ipn.ConfigVAlpha{
			Version:   "alpha0",
			AcceptDNS: "false",
			Hostname:  &opts.hostname,
			Locked:    "false",
			AuthKey:   ptr.To("secret-authkey"),
		}
		var routes []netip.Prefix
		if opts.subnetRoutes != "" || opts.isExitNode {
			r := opts.subnetRoutes
			if opts.isExitNode {
				r = "0.0.0.0/0,::/0," + r
			}
			for _, rr := range strings.Split(r, ",") {
				prefix, err := netip.ParsePrefix(rr)
				if err != nil {
					t.Fatal(err)
				}
				routes = append(routes, prefix)
			}
		}
		conf.AdvertiseRoutes = routes
		b, err := json.Marshal(conf)
		if err != nil {
			t.Fatalf("error marshalling tailscaled config")
		}
		mak.Set(&s.StringData, "tailscaled", string(b))
		labels["tailscale.com/parent-resource-ns"] = "" // Connector is cluster scoped
	}
	s.Labels = labels
	return s
}

func findGenName(t *testing.T, client client.Client, ns, name, typ string) (full, noSuffix string) {
	t.Helper()
	labels := map[string]string{
		LabelManaged:         "true",
		LabelParentName:      name,
		LabelParentNamespace: ns,
		LabelParentType:      typ,
	}
	s, err := getSingleObject[corev1.Secret](context.Background(), client, "operator-ns", labels)
	if err != nil {
		t.Fatalf("finding secret for %q: %v", name, err)
	}
	if s == nil {
		t.Fatalf("no secret found for %q %s %+#v", name, ns, labels)
	}
	return s.GetName(), strings.TrimSuffix(s.GetName(), "-0")
}

func mustCreate(t *testing.T, client client.Client, obj client.Object) {
	t.Helper()
	if err := client.Create(context.Background(), obj); err != nil {
		t.Fatalf("creating %q: %v", obj.GetName(), err)
	}
}

func mustUpdate[T any, O ptrObject[T]](t *testing.T, client client.Client, ns, name string, update func(O)) {
	t.Helper()
	obj := O(new(T))
	if err := client.Get(context.Background(), types.NamespacedName{
		Name:      name,
		Namespace: ns,
	}, obj); err != nil {
		t.Fatalf("getting %q: %v", name, err)
	}
	update(obj)
	if err := client.Update(context.Background(), obj); err != nil {
		t.Fatalf("updating %q: %v", name, err)
	}
}

func mustUpdateStatus[T any, O ptrObject[T]](t *testing.T, client client.Client, ns, name string, update func(O)) {
	t.Helper()
	obj := O(new(T))
	if err := client.Get(context.Background(), types.NamespacedName{
		Name:      name,
		Namespace: ns,
	}, obj); err != nil {
		t.Fatalf("getting %q: %v", name, err)
	}
	update(obj)
	if err := client.Status().Update(context.Background(), obj); err != nil {
		t.Fatalf("updating %q: %v", name, err)
	}
}

func expectEqual[T any, O ptrObject[T]](t *testing.T, client client.Client, want O) {
	t.Helper()
	got := O(new(T))
	if err := client.Get(context.Background(), types.NamespacedName{
		Name:      want.GetName(),
		Namespace: want.GetNamespace(),
	}, got); err != nil {
		t.Fatalf("getting %q: %v", want.GetName(), err)
	}
	// The resource version changes eagerly whenever the operator does even a
	// no-op update. Asserting a specific value leads to overly brittle tests,
	// so just remove it from both got and want.
	got.SetResourceVersion("")
	want.SetResourceVersion("")
	if diff := cmp.Diff(got, want); diff != "" {
		t.Fatalf("unexpected object (-got +want):\n%s", diff)
	}
}

func expectMissing[T any, O ptrObject[T]](t *testing.T, client client.Client, ns, name string) {
	t.Helper()
	obj := O(new(T))
	if err := client.Get(context.Background(), types.NamespacedName{
		Name:      name,
		Namespace: ns,
	}, obj); !apierrors.IsNotFound(err) {
		t.Fatalf("object %s/%s unexpectedly present, wanted missing", ns, name)
	}
}

func expectReconciled(t *testing.T, sr reconcile.Reconciler, ns, name string) {
	t.Helper()
	req := reconcile.Request{
		NamespacedName: types.NamespacedName{
			Namespace: ns,
			Name:      name,
		},
	}
	res, err := sr.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("Reconcile: unexpected error: %v", err)
	}
	if res.Requeue {
		t.Fatalf("unexpected immediate requeue")
	}
	if res.RequeueAfter != 0 {
		t.Fatalf("unexpected timed requeue (%v)", res.RequeueAfter)
	}
}

func expectRequeue(t *testing.T, sr reconcile.Reconciler, ns, name string) {
	t.Helper()
	req := reconcile.Request{
		NamespacedName: types.NamespacedName{
			Name:      name,
			Namespace: ns,
		},
	}
	res, err := sr.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("Reconcile: unexpected error: %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Fatalf("expected timed requeue, got success")
	}
}

type fakeTSClient struct {
	sync.Mutex
	keyRequests []tailscale.KeyCapabilities
	deleted     []string
}
type fakeTSNetServer struct {
	certDomains []string
}

func (f *fakeTSNetServer) CertDomains() []string {
	return f.certDomains
}

func (c *fakeTSClient) CreateKey(ctx context.Context, caps tailscale.KeyCapabilities) (string, *tailscale.Key, error) {
	c.Lock()
	defer c.Unlock()
	c.keyRequests = append(c.keyRequests, caps)
	k := &tailscale.Key{
		ID:           "key",
		Created:      time.Now(),
		Capabilities: caps,
	}
	return "secret-authkey", k, nil
}

func (c *fakeTSClient) DeleteDevice(ctx context.Context, deviceID string) error {
	c.Lock()
	defer c.Unlock()
	c.deleted = append(c.deleted, deviceID)
	return nil
}

func (c *fakeTSClient) KeyRequests() []tailscale.KeyCapabilities {
	c.Lock()
	defer c.Unlock()
	return c.keyRequests
}

func (c *fakeTSClient) Deleted() []string {
	c.Lock()
	defer c.Unlock()
	return c.deleted
}
