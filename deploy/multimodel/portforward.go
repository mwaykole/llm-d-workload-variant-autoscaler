package main

import (
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"os/exec"
	"time"

	"k8s.io/client-go/kubernetes"
)

// portForwarder wraps a kubectl port-forward process for service-level forwarding.
type portForwarder struct {
	cmd       *exec.Cmd
	localPort int
}

// newPortForwarder starts a kubectl port-forward to the named service.
// It picks a random local port to avoid conflicts.
func newPortForwarder(_ *kubernetes.Clientset, namespace, serviceName string, remotePort int) (*portForwarder, error) {
	localPort, err := freePort()
	if err != nil {
		return nil, fmt.Errorf("find free port: %w", err)
	}

	cmd := exec.Command("kubectl", "port-forward",
		"-n", namespace,
		fmt.Sprintf("svc/%s", serviceName),
		fmt.Sprintf("%d:%d", localPort, remotePort),
	)

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start port-forward: %w", err)
	}

	return &portForwarder{cmd: cmd, localPort: localPort}, nil
}

// waitReady polls until the local port accepts connections.
func (pf *portForwarder) waitReady(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("localhost:%d", pf.localPort), time.Second)
		if err == nil {
			conn.Close()
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("port-forward not ready after %s", timeout)
}

func (pf *portForwarder) close() {
	if pf.cmd != nil && pf.cmd.Process != nil {
		_ = pf.cmd.Process.Kill()
		_ = pf.cmd.Wait()
	}
}

// freePort asks the OS for an available port.
func freePort() (int, error) {
	l, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// httpGetStatus performs a GET request and returns the HTTP status code.
func httpGetStatus(url string) int {
	client := &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // local port-forward
		},
	}
	resp, err := client.Get(url)
	if err != nil {
		return 0
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

