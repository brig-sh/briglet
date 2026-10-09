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
	"strings"
	"testing"
)

func TestAddonObjects(t *testing.T) {
	objs, err := addonObjects("node1")
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, u := range objs {
		kinds = append(kinds, u.GetKind()+"/"+u.GetName())
		b, _ := u.MarshalJSON()
		if strings.Contains(string(b), "{{") {
			t.Errorf("%s/%s keeps a placeholder: %s", u.GetKind(), u.GetName(), b)
		}
	}
	want := "ClusterRoleBinding/briglet:node-proxier ServiceAccount/coredns ClusterRole/system:coredns " +
		"ClusterRoleBinding/system:coredns ConfigMap/coredns Deployment/coredns Service/kube-dns"
	if got := strings.Join(kinds, " "); got != want {
		t.Fatalf("objects\n got %s\nwant %s", got, want)
	}
	subj := objs[0].Object["subjects"].([]any)[0].(map[string]any)
	if subj["name"] != "system:node:node1" {
		t.Errorf("node-proxier subject = %v", subj)
	}
	cf := objs[4].Object["data"].(map[string]any)["Corefile"].(string)
	for _, s := range []string{"kubernetes cluster.local in-addr.arpa ip6.arpa", "forward . 10.87.0.1", "cache 30", "loop", "reload", "loadbalance"} {
		if !strings.Contains(cf, s) {
			t.Errorf("Corefile lacks %q:\n%s", s, cf)
		}
	}
	if ip := objs[6].Object["spec"].(map[string]any)["clusterIP"]; ip != ClusterDNSIP {
		t.Errorf("kube-dns clusterIP = %v", ip)
	}
}
