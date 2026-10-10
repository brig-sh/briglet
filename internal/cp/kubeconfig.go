// Copyright (c) 2026, NOFire AI
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package cp

import (
	"fmt"
	"os"
	"path/filepath"

	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

const clusterName = "briglet"

// WriteEgressConfig writes the apiserver's egress selector: cluster
// dials go through the node agent's CONNECT proxy on a unix socket,
// which carries them into the gateway's pod network.
func WriteEgressConfig(path, udsName string) error {
	y := `apiVersion: apiserver.k8s.io/v1beta1
kind: EgressSelectorConfiguration
egressSelections:
- name: cluster
  connection:
    proxyProtocol: HTTPConnect
    transport:
      uds:
        udsName: ` + udsName + `
`
	return os.WriteFile(path, []byte(y), 0o600)
}

// APIServerURL is the address host-side clients use.
func APIServerURL() string {
	return fmt.Sprintf("https://127.0.0.1:%d", APIServerPort)
}

// WriteKubeconfig writes a kubeconfig for the client cert pki/<certName>.{crt,key}
// with all PEM data embedded, so the file is self-contained.
func WriteKubeconfig(path, pkiDir, certName, user string) error {
	return WriteKubeconfigTo(path, pkiDir, certName, user, APIServerURL())
}

// WriteKubeconfigTo writes a kubeconfig with an explicit API server URL,
// for clients that reach the apiserver at another address, such as node
// agents joining from another host.
func WriteKubeconfigTo(path, pkiDir, certName, user, serverURL string) error {
	ca, err := readPEM(filepath.Join(pkiDir, "ca.crt"))
	if err != nil {
		return err
	}
	crt, err := readPEM(filepath.Join(pkiDir, certName+".crt"))
	if err != nil {
		return err
	}
	key, err := readPEM(filepath.Join(pkiDir, certName+".key"))
	if err != nil {
		return err
	}
	cfg := clientcmdapi.NewConfig()
	cfg.Clusters[clusterName] = &clientcmdapi.Cluster{Server: serverURL, CertificateAuthorityData: ca}
	cfg.AuthInfos[user] = &clientcmdapi.AuthInfo{ClientCertificateData: crt, ClientKeyData: key}
	ctx := user + "@" + clusterName
	cfg.Contexts[ctx] = &clientcmdapi.Context{Cluster: clusterName, AuthInfo: user}
	cfg.CurrentContext = ctx
	return clientcmd.WriteToFile(*cfg, path)
}

// WriteKubeconfigs writes the admin kubeconfig and one per component.
func WriteKubeconfigs(p Paths) error {
	for _, k := range []struct{ path, cert, user string }{
		{p.AdminKubeconfig, "admin", "kubernetes-admin"},
		{p.Kubeconfig("controller-manager"), "controller-manager", "system:kube-controller-manager"},
		{p.Kubeconfig("scheduler"), "scheduler", "system:kube-scheduler"},
		{p.Kubeconfig("node"), "node", "node"},
	} {
		if err := WriteKubeconfig(k.path, p.PKI, k.cert, k.user); err != nil {
			return fmt.Errorf("kubeconfig %s: %w", k.path, err)
		}
	}
	return nil
}
