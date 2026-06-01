package gitops

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/httpstream"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"
)

// argoServerRemotePort is the port the argocd-server container serves the web
// UI / API on (HTTPS by default).
const argoServerRemotePort = 8080

// StartArgoServerForward establishes a background port-forward to a running
// argocd-server pod and returns the local base URL (e.g. https://localhost:NNNNN)
// for the web UI. The forward stays alive until ctx is cancelled. The Argo CD
// server serves HTTPS with a self-signed certificate by default, so the browser
// may show a certificate warning.
func StartArgoServerForward(ctx context.Context, config *rest.Config, clientset kubernetes.Interface, namespace string) (string, error) {
	if namespace == "" {
		namespace = ArgoCDNamespace
	}

	pod, err := findArgoServerPod(ctx, clientset, namespace)
	if err != nil {
		return "", err
	}

	readyChan := make(chan struct{}, 1)
	errChan := make(chan error, 1)

	reqURL := clientset.CoreV1().RESTClient().Post().
		Resource("pods").Namespace(namespace).Name(pod).
		SubResource("portforward").URL()

	transport, upgrader, err := spdy.RoundTripperFor(config)
	if err != nil {
		return "", err
	}
	dialer := spdy.NewDialer(upgrader, &http.Client{Transport: transport}, http.MethodPost, reqURL)
	if tunneling, terr := portforward.NewSPDYOverWebsocketDialer(reqURL, config); terr == nil {
		dialer = portforward.NewFallbackDialer(tunneling, dialer, httpstream.IsUpgradeFailure)
	}

	ports := []string{fmt.Sprintf("0:%d", argoServerRemotePort)}
	forwarder, err := portforward.NewOnAddresses(dialer, []string{"localhost"}, ports, ctx.Done(), readyChan, nil, os.Stderr)
	if err != nil {
		return "", err
	}

	go func() {
		if ferr := forwarder.ForwardPorts(); ferr != nil {
			select {
			case errChan <- ferr:
			default:
			}
		}
	}()

	select {
	case <-readyChan:
		forwarded, perr := forwarder.GetPorts()
		if perr != nil {
			return "", perr
		}
		if len(forwarded) == 0 {
			return "", errors.New("no local port allocated")
		}
		return fmt.Sprintf("https://localhost:%d", forwarded[0].Local), nil
	case err := <-errChan:
		return "", err
	case <-time.After(10 * time.Second):
		return "", errors.New("timed out establishing port-forward to argocd-server")
	}
}

// ArgoAdminUsername is the built-in administrator account created by a fresh
// Argo CD install.
const ArgoAdminUsername = "admin"

// ArgoInitialAdminPassword reads the auto-generated administrator password from
// the argocd-initial-admin-secret in the given namespace. This secret is created
// on first install and may be deleted by the administrator afterwards, in which
// case a descriptive error is returned.
func ArgoInitialAdminPassword(ctx context.Context, clientset kubernetes.Interface, namespace string) (string, error) {
	if namespace == "" {
		namespace = ArgoCDNamespace
	}
	secret, err := clientset.CoreV1().Secrets(namespace).Get(ctx, "argocd-initial-admin-secret", metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return "", errors.New("argocd-initial-admin-secret not found — the initial password may have been changed or the secret deleted")
		}
		return "", err
	}
	pw, ok := secret.Data["password"]
	if !ok || len(pw) == 0 {
		return "", errors.New("argocd-initial-admin-secret has no password field")
	}
	return string(pw), nil
}

// findArgoServerPod returns the name of a running argocd-server pod.
func findArgoServerPod(ctx context.Context, clientset kubernetes.Interface, namespace string) (string, error) {
	selectors := []string{
		"app.kubernetes.io/name=argocd-server",
		"app.kubernetes.io/component=server,app.kubernetes.io/part-of=argocd",
		"app=argocd-server",
	}
	for _, sel := range selectors {
		pods, err := clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: sel})
		if err != nil {
			return "", err
		}
		for i := range pods.Items {
			if pods.Items[i].Status.Phase == corev1.PodRunning {
				return pods.Items[i].Name, nil
			}
		}
	}
	return "", errors.New("no running argocd-server pod found")
}
