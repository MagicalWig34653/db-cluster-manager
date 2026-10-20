// SPDX-License-Identifier: AGPL-3.0-only

package dbadmin

import (
	"reflect"
	"testing"
)

func TestBuildPMAServiceSpec(t *testing.T) {
	s, err := BuildPMAServiceSpec(PMAParams{Name: "pma-prod", Image: "phpmyadmin:5.2.1-apache", HostNode: "host-a",
		ProxyHost: "galera-prod-proxy", Network: "dbcm-prod", ClusterLabel: "prod", AbsoluteURI: "https://pma.example.org/"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(s.Constraints, []string{"node.hostname == host-a"}) || s.Replicas != 1 || s.TargetPort != 80 {
		t.Fatalf("%+v", s)
	}
	wantEnv := map[string]string{"PMA_HOST": "galera-prod-proxy", "PMA_PORT": "3306", "PMA_ARBITRARY": "0",
		"UPLOAD_LIMIT": "64M", "MAX_EXECUTION_TIME": "300", "PMA_ABSOLUTE_URI": "https://pma.example.org/"}
	if !reflect.DeepEqual(s.Env, wantEnv) {
		t.Fatalf("%+v", s.Env)
	}
	if s.Labels["dbcm.pma.auth"] != "signon" || s.Labels["dbcm.cluster"] != "prod" || !reflect.DeepEqual(s.Networks, []string{"dbcm-prod"}) {
		t.Fatalf("%+v", s.Labels)
	}
	if _, ok := s.Env["MYSQL_ROOT_PASSWORD"]; ok {
		t.Fatal("no credentials in spec")
	}
}

func TestBuildPMAServiceSpecRejects(t *testing.T) {
	ok := PMAParams{Name: "pma", Image: "phpmyadmin:5.2.1", HostNode: "h", ProxyHost: "proxy"}
	if _, err := BuildPMAServiceSpec(ok); err != nil {
		t.Fatal(err)
	}
	mut := []func(*PMAParams){
		func(p *PMAParams) { p.Image = "phpmyadmin" }, func(p *PMAParams) { p.Image = "phpmyadmin:latest" },
		func(p *PMAParams) { p.Image = "phpmyadmin:5 --privileged" }, func(p *PMAParams) { p.HostNode = "" },
		func(p *PMAParams) { p.HostNode = "h || node.role == manager" }, func(p *PMAParams) { p.ProxyHost = "a b" },
		func(p *PMAParams) { p.Name = "Bad Name" }, func(p *PMAParams) { p.ProxyPort = 70000 },
		func(p *PMAParams) { p.Auth = "none" }, func(p *PMAParams) { p.UploadLimit = "1G;rm" },
		func(p *PMAParams) { p.AbsoluteURI = "javascript:alert(1)" }, func(p *PMAParams) { p.MemoryBytes = 1 },
	}
	for i, m := range mut {
		p := ok
		m(&p)
		if _, err := BuildPMAServiceSpec(p); err == nil {
			t.Errorf("mutation %d accepted", i)
		}
	}
	p := ok
	p.Auth = PMAAuthCookie
	p.PublishPort = 8080
	s, err := BuildPMAServiceSpec(p)
	if err != nil || s.Labels["dbcm.pma.auth"] != "cookie" || s.PublishedPort != 8080 {
		t.Fatalf("%v %+v", err, s)
	}
}
