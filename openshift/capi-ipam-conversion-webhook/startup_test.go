/*
Copyright 2026 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	configv1 "github.com/openshift/api/config/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
)

func TestClusterTLSWebhookStarts(t *testing.T) {
	apiServer := testTLSAPIServer()
	apiServer.TypeMeta = metav1.TypeMeta{APIVersion: "config.openshift.io/v1", Kind: "APIServer"}
	apiServer.ResourceVersion = "1"
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var response interface{}
		switch r.URL.Path {
		case "/api":
			response = &metav1.APIVersions{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "APIVersions"}, Versions: []string{}}
		case "/apis":
			version := metav1.GroupVersionForDiscovery{GroupVersion: "config.openshift.io/v1", Version: "v1"}
			response = &metav1.APIGroupList{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "APIGroupList"}, Groups: []metav1.APIGroup{{Name: "config.openshift.io", Versions: []metav1.GroupVersionForDiscovery{version}, PreferredVersion: version}}}
		case "/apis/config.openshift.io/v1":
			response = &metav1.APIResourceList{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "APIResourceList"}, GroupVersion: "config.openshift.io/v1", APIResources: []metav1.APIResource{{Name: "apiservers", Kind: "APIServer", Verbs: []string{"get", "list", "watch"}}}}
		case "/apis/config.openshift.io/v1/apiservers/cluster":
			response = apiServer
		case "/apis/config.openshift.io/v1/apiservers":
			if r.URL.Query().Get("watch") == "true" {
				if r.URL.Query().Get("sendInitialEvents") == "true" {
					bookmark := apiServer.DeepCopy()
					bookmark.Annotations = map[string]string{"k8s.io/initial-events-end": "true"}
					for _, event := range []metav1.WatchEvent{
						{Type: "ADDED", Object: runtime.RawExtension{Object: apiServer}},
						{Type: "BOOKMARK", Object: runtime.RawExtension{Object: bookmark}},
					} {
						if err := json.NewEncoder(w).Encode(event); err != nil {
							t.Error(err)
							return
						}
					}
				}
				w.(http.Flusher).Flush()
				<-r.Context().Done()
				return
			}
			response = &configv1.APIServerList{TypeMeta: metav1.TypeMeta{APIVersion: "config.openshift.io/v1", Kind: "APIServerList"}, ListMeta: metav1.ListMeta{ResourceVersion: "1"}, Items: []configv1.APIServer{*apiServer}}
		default:
			http.NotFound(w, r)
			return
		}
		if err := json.NewEncoder(w).Encode(response); err != nil {
			t.Error(err)
		}
	}))
	defer api.Close()

	dir := t.TempDir()
	kubeconfig := filepath.Join(dir, "kubeconfig")
	if err := os.WriteFile(kubeconfig, []byte(fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
- name: test
  cluster:
    server: %s
contexts:
- name: test
  context:
    cluster: test
current-context: test
`, api.URL)), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KUBECONFIG", kubeconfig)
	certificateSource := httptest.NewTLSServer(http.NotFoundHandler())
	defer certificateSource.Close()
	certificate := certificateSource.TLS.Certificates[0]
	key, err := x509.MarshalPKCS8PrivateKey(certificate.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string][]byte{
		"tls.crt": pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate.Certificate[0]}),
		"tls.key": pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key}),
	} {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- run(ctx, options{port: port, certDir: dir}) }()
	defer func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(10 * time.Second):
			t.Error("conversion webhook did not shut down")
		}
	}()
	httpClient := certificateSource.Client()
	httpClient.Timeout = 100 * time.Millisecond
	err = wait.PollUntilContextTimeout(ctx, 20*time.Millisecond, 10*time.Second, true, func(ctx context.Context) (bool, error) {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+net.JoinHostPort("127.0.0.1", fmt.Sprint(port))+"/readyz", http.NoBody)
		if err != nil {
			return false, err
		}
		response, err := httpClient.Do(request)
		if err != nil {
			return false, nil
		}
		defer response.Body.Close()
		return response.StatusCode == http.StatusOK, nil
	})
	if err != nil {
		t.Fatalf("cluster-profile mode did not start the webhook listener: %v", err)
	}
}
