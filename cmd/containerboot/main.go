// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

//go:build linux

// The containerboot binary is a wrapper for starting tailscaled in a container.
// It handles reading the desired mode of operation out of environment
// variables, bringing up and authenticating Tailscale, and any other
// kubernetes-specific side jobs.
//
// As with most container things, configuration is passed through environment
// variables. All configuration is optional.
//
//   - TS_AUTHKEY: the authkey to use for login.
//   - TS_HOSTNAME: the hostname to request for the node.
//   - TS_ROUTES: subnet routes to advertise. Explicitly setting it to an empty
//     value will cause containerboot to stop acting as a subnet router for any
//     previously advertised routes. To accept routes, use TS_EXTRA_ARGS to pass
//     in --accept-routes.
//   - TS_DEST_IP: proxy all incoming Tailscale traffic to the given
//     destination.
//   - TS_TAILNET_TARGET_IP: proxy all incoming non-Tailscale traffic to the given
//     destination defined by an IP.
//   - TS_TAILNET_TARGET_FQDN: proxy all incoming non-Tailscale traffic to the given
//     destination defined by a MagicDNS name.
//   - TS_TAILSCALED_EXTRA_ARGS: extra arguments to 'tailscaled'.
//   - TS_EXTRA_ARGS: extra arguments to 'tailscale up'.
//   - TS_USERSPACE: run with userspace networking (the default)
//     instead of kernel networking.
//   - TS_STATE_DIR: the directory in which to store tailscaled
//     state. The data should persist across container
//     restarts.
//   - TS_ACCEPT_DNS: whether to use the tailnet's DNS configuration.
//   - TS_KUBE_SECRET: the name of the Kubernetes secret in which to
//     store tailscaled state.
//   - TS_SOCKS5_SERVER: the address on which to listen for SOCKS5
//     proxying into the tailnet.
//   - TS_OUTBOUND_HTTP_PROXY_LISTEN: the address on which to listen
//     for HTTP proxying into the tailnet.
//   - TS_SOCKET: the path where the tailscaled LocalAPI socket should
//     be created.
//   - TS_AUTH_ONCE: if true, only attempt to log in if not already
//     logged in. If false (the default, for backwards
//     compatibility), forcibly log in every time the
//     container starts.
//   - TS_SERVE_CONFIG: if specified, is the file path where the ipn.ServeConfig is located.
//     It will be applied once tailscaled is up and running. If the file contains
//     ${TS_CERT_DOMAIN}, it will be replaced with the value of the available FQDN.
//     It cannot be used in conjunction with TS_DEST_IP. The file is watched for changes,
//     and will be re-applied when it changes.
//   - EXPERIMENTAL_TS_CONFIGFILE_PATH: if specified, a path to tailscaled
//     config. If this is set, TS_HOSTNAME, TS_EXTRA_ARGS, TS_AUTHKEY,
//     TS_ROUTES, TS_ACCEPT_DNS env vars must not be set. If this is set,
//     containerboot only runs `tailscaled --config <path-to-this-configfile>`
//     and not `tailscale up` or `tailscale set`.
//     The config file contents are currently read once on container start.
//     NB: This env var is currently experimental and the logic will likely change!
//   - EXPERIMENTAL_ALLOW_PROXYING_CLUSTER_TRAFFIC_VIA_INGRESS: if set to true
//     and if this containerboot instance is an L7 ingress proxy (created by
//     the Kubernetes operator), set up rules to allow proxying cluster traffic,
//     received on the Pod IP of this node, to the ingress target in the cluster.
//     This, in conjunction with MagicDNS name resolution in cluster, can be
//     useful for cases where a cluster workload needs to access a target in
//     cluster using the same hostname (in this case, the MagicDNS name of the ingress proxy)
//     as a non-cluster workload on tailnet.
//     This is only meant to be configured by the Kubernetes operator.
//
// When running on Kubernetes, containerboot defaults to storing state in the
// "tailscale" kube secret. To store state on local disk instead, set
// TS_KUBE_SECRET="" and TS_STATE_DIR=/path/to/storage/dir. The state dir should
// be persistent storage.
//
// Additionally, if TS_AUTHKEY is not set and the TS_KUBE_SECRET contains an
// "authkey" field, that key is used as the tailscale authkey.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net/netip"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"
	"golang.org/x/sys/unix"
	"tailscale.com/client/tailscale"
	"tailscale.com/ipn"
	"tailscale.com/ipn/conffile"
	"tailscale.com/tailcfg"
	"tailscale.com/types/logger"
	"tailscale.com/types/ptr"
	"tailscale.com/util/deephash"
	"tailscale.com/util/linuxfw"
)

func newNetfilterRunner(logf logger.Logf) (linuxfw.NetfilterRunner, error) {
	if defaultBool("TS_TEST_FAKE_NETFILTER", false) {
		return linuxfw.NewFakeIPTablesRunner(), nil
	}
	return linuxfw.New(logf, "")
}

func main() {
	log.SetPrefix("boot: ")
	tailscale.I_Acknowledge_This_API_Is_Unstable = true
	cfg := &settings{
		AuthKey:                               defaultEnvs([]string{"TS_AUTHKEY", "TS_AUTH_KEY"}, ""),
		Hostname:                              defaultEnv("TS_HOSTNAME", ""),
		Routes:                                defaultEnvStringPointer("TS_ROUTES"),
		ServeConfigPath:                       defaultEnv("TS_SERVE_CONFIG", ""),
		ProxyTo:                               defaultEnv("TS_DEST_IP", ""),
		TailnetTargetIP:                       defaultEnv("TS_TAILNET_TARGET_IP", ""),
		TailnetTargetFQDN:                     defaultEnv("TS_TAILNET_TARGET_FQDN", ""),
		DaemonExtraArgs:                       defaultEnv("TS_TAILSCALED_EXTRA_ARGS", ""),
		ExtraArgs:                             defaultEnv("TS_EXTRA_ARGS", ""),
		InKubernetes:                          os.Getenv("KUBERNETES_SERVICE_HOST") != "",
		UserspaceMode:                         defaultBool("TS_USERSPACE", true),
		StateDir:                              defaultEnv("TS_STATE_DIR", ""),
		AcceptDNS:                             defaultEnvBoolPointer("TS_ACCEPT_DNS"),
		KubeSecret:                            defaultEnv("TS_KUBE_SECRET", "tailscale"),
		SOCKSProxyAddr:                        defaultEnv("TS_SOCKS5_SERVER", ""),
		HTTPProxyAddr:                         defaultEnv("TS_OUTBOUND_HTTP_PROXY_LISTEN", ""),
		Socket:                                defaultEnv("TS_SOCKET", "/tmp/tailscaled.sock"),
		AuthOnce:                              defaultBool("TS_AUTH_ONCE", false),
		Root:                                  defaultEnv("TS_TEST_ONLY_ROOT", "/"),
		TailscaledConfigFilePath:              defaultEnv("EXPERIMENTAL_TS_CONFIGFILE_PATH", ""),
		AllowProxyingClusterTrafficViaIngress: defaultBool("EXPERIMENTAL_ALLOW_PROXYING_CLUSTER_TRAFFIC_VIA_INGRESS", false),
		PodIP:                                 defaultEnv("POD_IP", ""),
	}

	if err := cfg.validate(); err != nil {
		log.Fatalf("invalid configuration: %v", err)
	}

	if !cfg.UserspaceMode {
		if err := ensureTunFile(cfg.Root); err != nil {
			log.Fatalf("Unable to create tuntap device file: %v", err)
		}
		if cfg.ProxyTo != "" || cfg.Routes != nil || cfg.TailnetTargetIP != "" || cfg.TailnetTargetFQDN != "" {
			if err := ensureIPForwarding(cfg.Root, cfg.ProxyTo, cfg.TailnetTargetIP, cfg.TailnetTargetFQDN, cfg.Routes); err != nil {
				log.Printf("Failed to enable IP forwarding: %v", err)
				log.Printf("To run tailscale as a proxy or router container, IP forwarding must be enabled.")
				if cfg.InKubernetes {
					log.Fatalf("You can either set the sysctls as a privileged initContainer, or run the tailscale container with privileged=true.")
				} else {
					log.Fatalf("You can fix this by running the container with privileged=true, or the equivalent in your container runtime that permits access to sysctls.")
				}
			}
		}
	}

	if cfg.InKubernetes {
		initKube(cfg.Root)
	}

	// Context is used for all setup stuff until we're in steady
	// state, so that if something is hanging we eventually time out
	// and crashloop the container.
	bootCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if cfg.InKubernetes && cfg.KubeSecret != "" {
		canPatch, err := kc.CheckSecretPermissions(bootCtx, cfg.KubeSecret)
		if err != nil {
			log.Fatalf("Some Kubernetes permissions are missing, please check your RBAC configuration: %v", err)
		}
		cfg.KubernetesCanPatch = canPatch

		if cfg.AuthKey == "" && !isOneStepConfig(cfg) {
			key, err := findKeyInKubeSecret(bootCtx, cfg.KubeSecret)
			if err != nil {
				log.Fatalf("Getting authkey from kube secret: %v", err)
			}
			if key != "" {
				// This behavior of pulling authkeys from kube secrets was added
				// at the same time as the patch permission, so we can enforce
				// that we must be able to patch out the authkey after
				// authenticating if you want to use this feature. This avoids
				// us having to deal with the case where we might leave behind
				// an unnecessary reusable authkey in a secret, like a rake in
				// the grass.
				if !cfg.KubernetesCanPatch {
					log.Fatalf("authkey found in TS_KUBE_SECRET, but the pod doesn't have patch permissions on the secret to manage the authkey.")
				}
				log.Print("Using authkey found in kube secret")
				cfg.AuthKey = key
			} else {
				log.Print("No authkey found in kube secret and TS_AUTHKEY not provided, login will be interactive if needed.")
			}
		}
	}

	client, daemonProcess, err := startTailscaled(bootCtx, cfg)
	if err != nil {
		log.Fatalf("failed to bring up tailscale: %v", err)
	}
	killTailscaled := func() {
		if err := daemonProcess.Signal(unix.SIGTERM); err != nil {
			log.Fatalf("error shutting tailscaled down: %v", err)
		}
	}
	defer killTailscaled()

	w, err := client.WatchIPNBus(bootCtx, ipn.NotifyInitialNetMap|ipn.NotifyInitialPrefs|ipn.NotifyInitialState)
	if err != nil {
		log.Fatalf("failed to watch tailscaled for updates: %v", err)
	}

	// Now that we've started tailscaled, we can symlink the socket to the
	// default location if needed.
	const defaultTailscaledSocketPath = "/var/run/tailscale/tailscaled.sock"
	if cfg.Socket != "" && cfg.Socket != defaultTailscaledSocketPath {
		// If we were given a socket path, symlink it to the default location so
		// that the CLI can find it without any extra flags.
		// See #6849.

		dir := filepath.Dir(defaultTailscaledSocketPath)
		err := os.MkdirAll(dir, 0700)
		if err == nil {
			err = syscall.Symlink(cfg.Socket, defaultTailscaledSocketPath)
		}
		if err != nil {
			log.Printf("[warning] failed to symlink socket: %v\n\tTo interact with the Tailscale CLI please use `tailscale --socket=%q`", err, cfg.Socket)
		}
	}

	// Because we're still shelling out to `tailscale up` to get access to its
	// flag parser, we have to stop watching the IPN bus so that we can block on
	// the subcommand without stalling anything. Then once it's done, we resume
	// watching the bus.
	//
	// Depending on the requested mode of operation, this auth step happens at
	// different points in containerboot's lifecycle, hence the helper function.
	didLogin := false
	authTailscale := func() error {
		if didLogin {
			return nil
		}
		didLogin = true
		w.Close()
		if err := tailscaleUp(bootCtx, cfg); err != nil {
			return fmt.Errorf("failed to auth tailscale: %v", err)
		}
		w, err = client.WatchIPNBus(bootCtx, ipn.NotifyInitialNetMap|ipn.NotifyInitialState)
		if err != nil {
			return fmt.Errorf("rewatching tailscaled for updates after auth: %v", err)
		}
		return nil
	}

	if isTwoStepConfigAlwaysAuth(cfg) {
		if err := authTailscale(); err != nil {
			log.Fatalf("failed to auth tailscale: %v", err)
		}
	}

authLoop:
	for {
		n, err := w.Next()
		if err != nil {
			log.Fatalf("failed to read from tailscaled: %v", err)
		}

		if n.State != nil {
			switch *n.State {
			case ipn.NeedsLogin:
				if isOneStepConfig(cfg) {
					// This could happen if this is the
					// first time tailscaled was run for
					// this device and the auth key was not
					// passed via the configfile.
					log.Fatalf("invalid state: tailscaled daemon started with a config file, but tailscale is not logged in: ensure you pass a valid auth key in the config file.")
				}
				if err := authTailscale(); err != nil {
					log.Fatalf("failed to auth tailscale: %v", err)
				}
			case ipn.NeedsMachineAuth:
				log.Printf("machine authorization required, please visit the admin panel")
			case ipn.Running:
				// Technically, all we want is to keep monitoring the bus for
				// netmap updates. However, in order to make the container crash
				// if tailscale doesn't initially come up, the watch has a
				// startup deadline on it. So, we have to break out of this
				// watch loop, cancel the watch, and watch again with no
				// deadline to continue monitoring for changes.
				break authLoop
			default:
				log.Printf("tailscaled in state %q, waiting", *n.State)
			}
		}
	}

	w.Close()

	ctx, cancel := contextWithExitSignalWatch()
	defer cancel()

	if isTwoStepConfigAuthOnce(cfg) {
		// Now that we are authenticated, we can set/reset any of the
		// settings that we need to.
		if err := tailscaleSet(ctx, cfg); err != nil {
			log.Fatalf("failed to auth tailscale: %v", err)
		}
	}

	if cfg.ServeConfigPath != "" {
		// Remove any serve config that may have been set by a previous run of
		// containerboot, but only if we're providing a new one.
		if err := client.SetServeConfig(ctx, new(ipn.ServeConfig)); err != nil {
			log.Fatalf("failed to unset serve config: %v", err)
		}
	}

	if cfg.InKubernetes && cfg.KubeSecret != "" && cfg.KubernetesCanPatch && isTwoStepConfigAuthOnce(cfg) {
		// We were told to only auth once, so any secret-bound
		// authkey is no longer needed. We don't strictly need to
		// wipe it, but it's good hygiene.
		log.Printf("Deleting authkey from kube secret")
		if err := deleteAuthKey(ctx, cfg.KubeSecret); err != nil {
			log.Fatalf("deleting authkey from kube secret: %v", err)
		}
	}

	w, err = client.WatchIPNBus(ctx, ipn.NotifyInitialNetMap|ipn.NotifyInitialState)
	if err != nil {
		log.Fatalf("rewatching tailscaled for updates after auth: %v", err)
	}

	var (
		wantProxy         = cfg.ProxyTo != "" || cfg.TailnetTargetIP != "" || cfg.TailnetTargetFQDN != "" || cfg.AllowProxyingClusterTrafficViaIngress
		wantDeviceInfo    = cfg.InKubernetes && cfg.KubeSecret != "" && cfg.KubernetesCanPatch
		startupTasksDone  = false
		currentIPs        deephash.Sum // tailscale IPs assigned to device
		currentDeviceInfo deephash.Sum // device ID and fqdn

		currentEgressIPs deephash.Sum

		certDomain        = new(atomic.Pointer[string])
		certDomainChanged = make(chan bool, 1)
	)
	if cfg.ServeConfigPath != "" {
		go watchServeConfigChanges(ctx, cfg.ServeConfigPath, certDomainChanged, certDomain, client)
	}
	var nfr linuxfw.NetfilterRunner
	if wantProxy {
		nfr, err = newNetfilterRunner(log.Printf)
		if err != nil {
			log.Fatalf("error creating new netfilter runner: %v", err)
		}
	}
	notifyChan := make(chan ipn.Notify)
	errChan := make(chan error)
	go func() {
		for {
			n, err := w.Next()
			if err != nil {
				errChan <- err
				break
			} else {
				notifyChan <- n
			}
		}
	}()
	var wg sync.WaitGroup

runLoop:
	for {
		select {
		case <-ctx.Done():
			// Although killTailscaled() is deferred earlier, if we
			// have started the reaper defined below, we need to
			// kill tailscaled and let reaper clean up child
			// processes.
			killTailscaled()
			break runLoop
		case err := <-errChan:
			log.Fatalf("failed to read from tailscaled: %v", err)
		case n := <-notifyChan:
			if n.State != nil && *n.State != ipn.Running {
				// Something's gone wrong and we've left the authenticated state.
				// Our container image never recovered gracefully from this, and the
				// control flow required to make it work now is hard. So, just crash
				// the container and rely on the container runtime to restart us,
				// whereupon we'll go through initial auth again.
				log.Fatalf("tailscaled left running state (now in state %q), exiting", *n.State)
			}
			if n.NetMap != nil {
				addrs := n.NetMap.SelfNode.Addresses().AsSlice()
				newCurrentIPs := deephash.Hash(&addrs)
				ipsHaveChanged := newCurrentIPs != currentIPs

				if cfg.TailnetTargetFQDN != "" {
					var (
						egressAddrs          []netip.Prefix
						newCurentEgressIPs   deephash.Sum
						egressIPsHaveChanged bool
						node                 tailcfg.NodeView
						nodeFound            bool
					)
					for _, n := range n.NetMap.Peers {
						if strings.EqualFold(n.Name(), cfg.TailnetTargetFQDN) {
							node = n
							nodeFound = true
							break
						}
					}
					if !nodeFound {
						log.Printf("Tailscale node %q not found; it either does not exist, or not reachable because of ACLs", cfg.TailnetTargetFQDN)
						break
					}
					egressAddrs = node.Addresses().AsSlice()
					newCurentEgressIPs = deephash.Hash(&egressAddrs)
					egressIPsHaveChanged = newCurentEgressIPs != currentEgressIPs
					if egressIPsHaveChanged && len(egressAddrs) > 0 {
						for _, egressAddr := range egressAddrs {
							ea := egressAddr.Addr()
							// TODO (irbekrm): make it work for IPv6 too.
							if ea.Is6() {
								log.Println("Not installing egress forwarding rules for IPv6 as this is currently not supported")
								continue
							}
							log.Printf("Installing forwarding rules for destination %v", ea.String())
							if err := installEgressForwardingRule(ctx, ea.String(), addrs, nfr); err != nil {
								log.Fatalf("installing egress proxy rules for destination %s: %v", ea.String(), err)
							}
						}
					}
					currentEgressIPs = newCurentEgressIPs
				}
				if cfg.ProxyTo != "" && len(addrs) > 0 && ipsHaveChanged {
					log.Printf("Installing proxy rules")
					if err := installIngressForwardingRule(ctx, cfg.ProxyTo, addrs, nfr); err != nil {
						log.Fatalf("installing ingress proxy rules: %v", err)
					}
				}
				if cfg.ServeConfigPath != "" && len(n.NetMap.DNS.CertDomains) > 0 {
					cd := n.NetMap.DNS.CertDomains[0]
					prev := certDomain.Swap(ptr.To(cd))
					if prev == nil || *prev != cd {
						select {
						case certDomainChanged <- true:
						default:
						}
					}
				}
				if cfg.TailnetTargetIP != "" && ipsHaveChanged && len(addrs) > 0 {
					log.Printf("Installing forwarding rules for destination %v", cfg.TailnetTargetIP)
					if err := installEgressForwardingRule(ctx, cfg.TailnetTargetIP, addrs, nfr); err != nil {
						log.Fatalf("installing egress proxy rules: %v", err)
					}
				}
				// If this is a L7 cluster ingress proxy (set up
				// by Kubernetes operator) and proxying of
				// cluster traffic to the ingress target is
				// enabled, set up proxy rule each time the
				// tailnet IPs of this node change (including
				// the first time they become available).
				if cfg.AllowProxyingClusterTrafficViaIngress && cfg.ServeConfigPath != "" && ipsHaveChanged && len(addrs) > 0 {
					log.Printf("installing rules to forward traffic for %s to node's tailnet IP", cfg.PodIP)
					if err := installTSForwardingRuleForDestination(ctx, cfg.PodIP, addrs, nfr); err != nil {
						log.Fatalf("installing rules to forward traffic to node's tailnet IP: %v", err)
					}
				}
				currentIPs = newCurrentIPs

				deviceInfo := []any{n.NetMap.SelfNode.StableID(), n.NetMap.SelfNode.Name()}
				if cfg.InKubernetes && cfg.KubernetesCanPatch && cfg.KubeSecret != "" && deephash.Update(&currentDeviceInfo, &deviceInfo) {
					if err := storeDeviceInfo(ctx, cfg.KubeSecret, n.NetMap.SelfNode.StableID(), n.NetMap.SelfNode.Name(), n.NetMap.SelfNode.Addresses().AsSlice()); err != nil {
						log.Fatalf("storing device ID in kube secret: %v", err)
					}
				}
			}
			if !startupTasksDone {
				if (!wantProxy || currentIPs != deephash.Sum{}) && (!wantDeviceInfo || currentDeviceInfo != deephash.Sum{}) {
					// This log message is used in tests to detect when all
					// post-auth configuration is done.
					log.Println("Startup complete, waiting for shutdown signal")
					startupTasksDone = true

					// Reap all processes, since we are PID1 and need to collect zombies. We can
					// only start doing this once we've stopped shelling out to things
					// `tailscale up`, otherwise this goroutine can reap the CLI subprocesses
					// and wedge bringup.
					reaper := func() {
						defer wg.Done()
						for {
							var status unix.WaitStatus
							pid, err := unix.Wait4(-1, &status, 0, nil)
							if errors.Is(err, unix.EINTR) {
								continue
							}
							if err != nil {
								log.Fatalf("Waiting for exited processes: %v", err)
							}
							if pid == daemonProcess.Pid {
								log.Printf("Tailscaled exited")
								os.Exit(0)
							}
						}

					}
					wg.Add(1)
					go reaper()
				}
			}
		}
	}
	wg.Wait()
}

// watchServeConfigChanges watches path for changes, and when it sees one, reads
// the serve config from it, replacing ${TS_CERT_DOMAIN} with certDomain, and
// applies it to lc. It exits when ctx is canceled. cdChanged is a channel that
// is written to when the certDomain changes, causing the serve config to be
// re-read and applied.
func watchServeConfigChanges(ctx context.Context, path string, cdChanged <-chan bool, certDomainAtomic *atomic.Pointer[string], lc *tailscale.LocalClient) {
	if certDomainAtomic == nil {
		panic("cd must not be nil")
	}
	var tickChan <-chan time.Time
	var eventChan <-chan fsnotify.Event
	if w, err := fsnotify.NewWatcher(); err != nil {
		log.Printf("failed to create fsnotify watcher, timer-only mode: %v", err)
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		tickChan = ticker.C
	} else {
		defer w.Close()
		if err := w.Add(filepath.Dir(path)); err != nil {
			log.Fatalf("failed to add fsnotify watch: %v", err)
		}
		eventChan = w.Events
	}

	var certDomain string
	var prevServeConfig *ipn.ServeConfig
	for {
		select {
		case <-ctx.Done():
			return
		case <-cdChanged:
			certDomain = *certDomainAtomic.Load()
		case <-tickChan:
		case <-eventChan:
			// We can't do any reasonable filtering on the event because of how
			// k8s handles these mounts. So just re-read the file and apply it
			// if it's changed.
		}
		if certDomain == "" {
			continue
		}
		sc, err := readServeConfig(path, certDomain)
		if err != nil {
			log.Fatalf("failed to read serve config: %v", err)
		}
		if prevServeConfig != nil && reflect.DeepEqual(sc, prevServeConfig) {
			continue
		}
		log.Printf("Applying serve config")
		if err := lc.SetServeConfig(ctx, sc); err != nil {
			log.Fatalf("failed to set serve config: %v", err)
		}
		prevServeConfig = sc
	}
}

// readServeConfig reads the ipn.ServeConfig from path, replacing
// ${TS_CERT_DOMAIN} with certDomain.
func readServeConfig(path, certDomain string) (*ipn.ServeConfig, error) {
	if path == "" {
		return nil, nil
	}
	j, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	j = bytes.ReplaceAll(j, []byte("${TS_CERT_DOMAIN}"), []byte(certDomain))
	var sc ipn.ServeConfig
	if err := json.Unmarshal(j, &sc); err != nil {
		return nil, err
	}
	return &sc, nil
}

func startTailscaled(ctx context.Context, cfg *settings) (*tailscale.LocalClient, *os.Process, error) {
	args := tailscaledArgs(cfg)
	// tailscaled runs without context, since it needs to persist
	// beyond the startup timeout in ctx.
	cmd := exec.Command("tailscaled", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true,
	}
	log.Printf("Starting tailscaled")
	if err := cmd.Start(); err != nil {
		return nil, nil, fmt.Errorf("starting tailscaled failed: %v", err)
	}

	// Wait for the socket file to appear, otherwise API ops will racily fail.
	log.Printf("Waiting for tailscaled socket")
	for {
		if ctx.Err() != nil {
			log.Fatalf("Timed out waiting for tailscaled socket")
		}
		_, err := os.Stat(cfg.Socket)
		if errors.Is(err, fs.ErrNotExist) {
			time.Sleep(100 * time.Millisecond)
			continue
		} else if err != nil {
			log.Fatalf("Waiting for tailscaled socket: %v", err)
		}
		break
	}

	tsClient := &tailscale.LocalClient{
		Socket:        cfg.Socket,
		UseSocketOnly: true,
	}

	return tsClient, cmd.Process, nil
}

// tailscaledArgs uses cfg to construct the argv for tailscaled.
func tailscaledArgs(cfg *settings) []string {
	args := []string{"--socket=" + cfg.Socket}
	switch {
	case cfg.InKubernetes && cfg.KubeSecret != "":
		args = append(args, "--state=kube:"+cfg.KubeSecret)
		if cfg.StateDir == "" {
			cfg.StateDir = "/tmp"
		}
		fallthrough
	case cfg.StateDir != "":
		args = append(args, "--statedir="+cfg.StateDir)
	default:
		args = append(args, "--state=mem:", "--statedir=/tmp")
	}

	if cfg.UserspaceMode {
		args = append(args, "--tun=userspace-networking")
	} else if err := ensureTunFile(cfg.Root); err != nil {
		log.Fatalf("ensuring that /dev/net/tun exists: %v", err)
	}

	if cfg.SOCKSProxyAddr != "" {
		args = append(args, "--socks5-server="+cfg.SOCKSProxyAddr)
	}
	if cfg.HTTPProxyAddr != "" {
		args = append(args, "--outbound-http-proxy-listen="+cfg.HTTPProxyAddr)
	}
	if cfg.TailscaledConfigFilePath != "" {
		args = append(args, "--config="+cfg.TailscaledConfigFilePath)
	}
	if cfg.DaemonExtraArgs != "" {
		args = append(args, strings.Fields(cfg.DaemonExtraArgs)...)
	}
	return args
}

// tailscaleUp uses cfg to run 'tailscale up' everytime containerboot starts, or
// if TS_AUTH_ONCE is set, only the first time containerboot starts.
func tailscaleUp(ctx context.Context, cfg *settings) error {
	args := []string{"--socket=" + cfg.Socket, "up"}
	if cfg.AcceptDNS != nil && *cfg.AcceptDNS {
		args = append(args, "--accept-dns=true")
	} else {
		args = append(args, "--accept-dns=false")
	}
	if cfg.AuthKey != "" {
		args = append(args, "--authkey="+cfg.AuthKey)
	}
	// --advertise-routes can be passed an empty string to configure a
	// device (that might have previously advertised subnet routes) to not
	// advertise any routes. Respect an empty string passed by a user and
	// use it to explicitly unset the routes.
	if cfg.Routes != nil {
		args = append(args, "--advertise-routes="+*cfg.Routes)
	}
	if cfg.Hostname != "" {
		args = append(args, "--hostname="+cfg.Hostname)
	}
	if cfg.ExtraArgs != "" {
		args = append(args, strings.Fields(cfg.ExtraArgs)...)
	}
	log.Printf("Running 'tailscale up'")
	cmd := exec.CommandContext(ctx, "tailscale", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("tailscale up failed: %v", err)
	}
	return nil
}

// tailscaleSet uses cfg to run 'tailscale set' to set any known configuration
// options that are passed in via environment variables. This is run after the
// node is in Running state and only if TS_AUTH_ONCE is set.
func tailscaleSet(ctx context.Context, cfg *settings) error {
	args := []string{"--socket=" + cfg.Socket, "set"}
	if cfg.AcceptDNS != nil && *cfg.AcceptDNS {
		args = append(args, "--accept-dns=true")
	} else {
		args = append(args, "--accept-dns=false")
	}
	// --advertise-routes can be passed an empty string to configure a
	// device (that might have previously advertised subnet routes) to not
	// advertise any routes. Respect an empty string passed by a user and
	// use it to explicitly unset the routes.
	if cfg.Routes != nil {
		args = append(args, "--advertise-routes="+*cfg.Routes)
	}
	if cfg.Hostname != "" {
		args = append(args, "--hostname="+cfg.Hostname)
	}
	log.Printf("Running 'tailscale set'")
	cmd := exec.CommandContext(ctx, "tailscale", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("tailscale set failed: %v", err)
	}
	return nil
}

// ensureTunFile checks that /dev/net/tun exists, creating it if
// missing.
func ensureTunFile(root string) error {
	// Verify that /dev/net/tun exists, in some container envs it
	// needs to be mknod-ed.
	if _, err := os.Stat(filepath.Join(root, "dev/net")); errors.Is(err, fs.ErrNotExist) {
		if err := os.MkdirAll(filepath.Join(root, "dev/net"), 0755); err != nil {
			return err
		}
	}
	if _, err := os.Stat(filepath.Join(root, "dev/net/tun")); errors.Is(err, fs.ErrNotExist) {
		dev := unix.Mkdev(10, 200) // tuntap major and minor
		if err := unix.Mknod(filepath.Join(root, "dev/net/tun"), 0600|unix.S_IFCHR, int(dev)); err != nil {
			return err
		}
	}
	return nil
}

// ensureIPForwarding enables IPv4/IPv6 forwarding for the container.
func ensureIPForwarding(root, clusterProxyTarget, tailnetTargetiP, tailnetTargetFQDN string, routes *string) error {
	var (
		v4Forwarding, v6Forwarding bool
	)
	if clusterProxyTarget != "" {
		proxyIP, err := netip.ParseAddr(clusterProxyTarget)
		if err != nil {
			return fmt.Errorf("invalid cluster destination IP: %v", err)
		}
		if proxyIP.Is4() {
			v4Forwarding = true
		} else {
			v6Forwarding = true
		}
	}
	if tailnetTargetiP != "" {
		proxyIP, err := netip.ParseAddr(tailnetTargetiP)
		if err != nil {
			return fmt.Errorf("invalid tailnet destination IP: %v", err)
		}
		if proxyIP.Is4() {
			v4Forwarding = true
		} else {
			v6Forwarding = true
		}
	}
	// Currently we only proxy traffic to the IPv4 address of the tailnet
	// target.
	if tailnetTargetFQDN != "" {
		v4Forwarding = true
	}
	if routes != nil && *routes != "" {
		for _, route := range strings.Split(*routes, ",") {
			cidr, err := netip.ParsePrefix(route)
			if err != nil {
				return fmt.Errorf("invalid subnet route: %v", err)
			}
			if cidr.Addr().Is4() {
				v4Forwarding = true
			} else {
				v6Forwarding = true
			}
		}
	}

	var paths []string
	if v4Forwarding {
		paths = append(paths, filepath.Join(root, "proc/sys/net/ipv4/ip_forward"))
	}
	if v6Forwarding {
		paths = append(paths, filepath.Join(root, "proc/sys/net/ipv6/conf/all/forwarding"))
	}

	// In some common configurations (e.g. default docker,
	// kubernetes), the container environment denies write access to
	// most sysctls, including IP forwarding controls. Check the
	// sysctl values before trying to change them, so that we
	// gracefully do nothing if the container's already been set up
	// properly by e.g. a k8s initContainer.
	for _, path := range paths {
		bs, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("reading %q: %w", path, err)
		}
		if v := strings.TrimSpace(string(bs)); v != "1" {
			if err := os.WriteFile(path, []byte("1"), 0644); err != nil {
				return fmt.Errorf("enabling %q: %w", path, err)
			}
		}
	}
	return nil
}

func installEgressForwardingRule(ctx context.Context, dstStr string, tsIPs []netip.Prefix, nfr linuxfw.NetfilterRunner) error {
	dst, err := netip.ParseAddr(dstStr)
	if err != nil {
		return err
	}
	var local netip.Addr
	for _, pfx := range tsIPs {
		if !pfx.IsSingleIP() {
			continue
		}
		if pfx.Addr().Is4() != dst.Is4() {
			continue
		}
		local = pfx.Addr()
		break
	}
	if !local.IsValid() {
		return fmt.Errorf("no tailscale IP matching family of %s found in %v", dstStr, tsIPs)
	}
	if err := nfr.DNATNonTailscaleTraffic("tailscale0", dst); err != nil {
		return fmt.Errorf("installing egress proxy rules: %w", err)
	}
	if err := nfr.AddSNATRuleForDst(local, dst); err != nil {
		return fmt.Errorf("installing egress proxy rules: %w", err)
	}
	if err := nfr.ClampMSSToPMTU("tailscale0", dst); err != nil {
		return fmt.Errorf("installing egress proxy rules: %w", err)
	}
	return nil
}

// installTSForwardingRuleForDestination accepts a destination address and a
// list of node's tailnet addresses, sets up rules to forward traffic for
// destination to the tailnet IP matching the destination IP family.
// Destination can be Pod IP of this node.
func installTSForwardingRuleForDestination(ctx context.Context, dstFilter string, tsIPs []netip.Prefix, nfr linuxfw.NetfilterRunner) error {
	dst, err := netip.ParseAddr(dstFilter)
	if err != nil {
		return err
	}
	var local netip.Addr
	for _, pfx := range tsIPs {
		if !pfx.IsSingleIP() {
			continue
		}
		if pfx.Addr().Is4() != dst.Is4() {
			continue
		}
		local = pfx.Addr()
		break
	}
	if !local.IsValid() {
		return fmt.Errorf("no tailscale IP matching family of %s found in %v", dstFilter, tsIPs)
	}
	if err := nfr.AddDNATRule(dst, local); err != nil {
		return fmt.Errorf("installing rule for forwarding traffic to tailnet IP: %w", err)
	}
	return nil
}

func installIngressForwardingRule(ctx context.Context, dstStr string, tsIPs []netip.Prefix, nfr linuxfw.NetfilterRunner) error {
	dst, err := netip.ParseAddr(dstStr)
	if err != nil {
		return err
	}
	var local netip.Addr
	for _, pfx := range tsIPs {
		if !pfx.IsSingleIP() {
			continue
		}
		if pfx.Addr().Is4() != dst.Is4() {
			continue
		}
		local = pfx.Addr()
		break
	}
	if !local.IsValid() {
		return fmt.Errorf("no tailscale IP matching family of %s found in %v", dstStr, tsIPs)
	}
	if err := nfr.AddDNATRule(local, dst); err != nil {
		return fmt.Errorf("installing ingress proxy rules: %w", err)
	}
	if err := nfr.ClampMSSToPMTU("tailscale0", dst); err != nil {
		return fmt.Errorf("installing ingress proxy rules: %w", err)
	}
	return nil
}

// settings is all the configuration for containerboot.
type settings struct {
	AuthKey  string
	Hostname string
	Routes   *string
	// ProxyTo is the destination IP to which all incoming
	// Tailscale traffic should be proxied. If empty, no proxying
	// is done. This is typically a locally reachable IP.
	ProxyTo string
	// TailnetTargetIP is the destination IP to which all incoming
	// non-Tailscale traffic should be proxied. This is typically a
	// Tailscale IP.
	TailnetTargetIP string
	// TailnetTargetFQDN is an MagicDNS name to which all incoming
	// non-Tailscale traffic should be proxied. This must be a full Tailnet
	// node FQDN.
	TailnetTargetFQDN        string
	ServeConfigPath          string
	DaemonExtraArgs          string
	ExtraArgs                string
	InKubernetes             bool
	UserspaceMode            bool
	StateDir                 string
	AcceptDNS                *bool
	KubeSecret               string
	SOCKSProxyAddr           string
	HTTPProxyAddr            string
	Socket                   string
	AuthOnce                 bool
	Root                     string
	KubernetesCanPatch       bool
	TailscaledConfigFilePath string
	// If set to true and, if this containerboot instance is a Kubernetes
	// ingress proxy, set up rules to forward incoming cluster traffic to be
	// forwarded to the ingress target in cluster.
	AllowProxyingClusterTrafficViaIngress bool
	// PodIP is the IP of the Pod if running in Kubernetes. This is used
	// when setting up rules to proxy cluster traffic to cluster ingress
	// target.
	PodIP string
}

func (s *settings) validate() error {
	if s.TailscaledConfigFilePath != "" {
		if _, err := conffile.Load(s.TailscaledConfigFilePath); err != nil {
			return fmt.Errorf("error validating tailscaled configfile contents: %w", err)
		}
	}
	if s.ProxyTo != "" && s.UserspaceMode {
		return errors.New("TS_DEST_IP is not supported with TS_USERSPACE")
	}
	if s.TailnetTargetIP != "" && s.UserspaceMode {
		return errors.New("TS_TAILNET_TARGET_IP is not supported with TS_USERSPACE")
	}
	if s.TailnetTargetFQDN != "" && s.UserspaceMode {
		return errors.New("TS_TAILNET_TARGET_FQDN is not supported with TS_USERSPACE")
	}
	if s.TailnetTargetFQDN != "" && s.TailnetTargetIP != "" {
		return errors.New("Both TS_TAILNET_TARGET_IP and TS_TAILNET_FQDN cannot be set")
	}
	if s.TailscaledConfigFilePath != "" && (s.AcceptDNS != nil || s.AuthKey != "" || s.Routes != nil || s.ExtraArgs != "" || s.Hostname != "") {
		return errors.New("EXPERIMENTAL_TS_CONFIGFILE_PATH cannot be set in combination with TS_HOSTNAME, TS_EXTRA_ARGS, TS_AUTHKEY, TS_ROUTES, TS_ACCEPT_DNS.")
	}
	if s.AllowProxyingClusterTrafficViaIngress && s.UserspaceMode {
		return errors.New("EXPERIMENTAL_ALLOW_PROXYING_CLUSTER_TRAFFIC_VIA_INGRESS is not supported in userspace mode")
	}
	if s.AllowProxyingClusterTrafficViaIngress && s.ServeConfigPath == "" {
		return errors.New("EXPERIMENTAL_ALLOW_PROXYING_CLUSTER_TRAFFIC_VIA_INGRESS is set but this is not a cluster ingress proxy")
	}
	if s.AllowProxyingClusterTrafficViaIngress && s.PodIP == "" {
		return errors.New("EXPERIMENTAL_ALLOW_PROXYING_CLUSTER_TRAFFIC_VIA_INGRESS is set but POD_IP is not set")
	}
	return nil
}

// defaultEnv returns the value of the given envvar name, or defVal if
// unset.
func defaultEnv(name, defVal string) string {
	if v, ok := os.LookupEnv(name); ok {
		return v
	}
	return defVal
}

// defaultEnvStringPointer returns a pointer to the given envvar value if set, else
// returns nil. This is useful in cases where we need to distinguish between a
// variable being set to empty string vs unset.
func defaultEnvStringPointer(name string) *string {
	if v, ok := os.LookupEnv(name); ok {
		return &v
	}
	return nil
}

// defaultEnvBoolPointer returns a pointer to the given envvar value if set, else
// returns nil. This is useful in cases where we need to distinguish between a
// variable being explicitly set to false vs unset.
func defaultEnvBoolPointer(name string) *bool {
	v := os.Getenv(name)
	ret, err := strconv.ParseBool(v)
	if err != nil {
		return nil
	}
	return &ret
}

func defaultEnvs(names []string, defVal string) string {
	for _, name := range names {
		if v, ok := os.LookupEnv(name); ok {
			return v
		}
	}
	return defVal
}

// defaultBool returns the boolean value of the given envvar name, or
// defVal if unset or not a bool.
func defaultBool(name string, defVal bool) bool {
	v := os.Getenv(name)
	ret, err := strconv.ParseBool(v)
	if err != nil {
		return defVal
	}
	return ret
}

// contextWithExitSignalWatch watches for SIGTERM/SIGINT signals. It returns a
// context that gets cancelled when a signal is received and a cancel function
// that can be called to free the resources when the watch should be stopped.
func contextWithExitSignalWatch() (context.Context, func()) {
	closeChan := make(chan string)
	ctx, cancel := context.WithCancel(context.Background())
	signalChan := make(chan os.Signal, 1)
	signal.Notify(signalChan, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		select {
		case <-signalChan:
			cancel()
		case <-closeChan:
			return
		}
	}()
	f := func() {
		closeChan <- "goodbye"
	}
	return ctx, f
}

// isTwoStepConfigAuthOnce returns true if the Tailscale node should be configured
// in two steps and login should only happen once.
// Step 1: run 'tailscaled'
// Step 2):
// A) if this is the first time starting this node run 'tailscale up --authkey <authkey> <config opts>'
// B) if this is not the first time starting this node run 'tailscale set <config opts>'.
func isTwoStepConfigAuthOnce(cfg *settings) bool {
	return cfg.AuthOnce && cfg.TailscaledConfigFilePath == ""
}

// isTwoStepConfigAlwaysAuth returns true if the Tailscale node should be configured
// in two steps and we should log in every time it starts.
// Step 1: run 'tailscaled'
// Step 2): run 'tailscale up --authkey <authkey> <config opts>'
func isTwoStepConfigAlwaysAuth(cfg *settings) bool {
	return !cfg.AuthOnce && cfg.TailscaledConfigFilePath == ""
}

// isOneStepConfig returns true if the Tailscale node should always be ran and
// configured in a single step by running 'tailscaled <config opts>'
func isOneStepConfig(cfg *settings) bool {
	return cfg.TailscaledConfigFilePath != ""
}
