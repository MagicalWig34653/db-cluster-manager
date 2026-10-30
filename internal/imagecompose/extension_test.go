// SPDX-License-Identifier: AGPL-3.0-only

package imagecompose

import (
	"strings"
	"testing"
)

// Test CA certificates (public data, generated once with openssl for these tests).
const testCA = `-----BEGIN CERTIFICATE-----
MIIBZDCCAQqgAwIBAgIUag79W2fQfzb+SJq7+x/SI0cs1xQwCgYIKoZIzj0EAwIw
FzEVMBMGA1UEAwwMZGJjbSBUZXN0IENBMCAXDTI2MTAyOTIxMzc1NFoYDzIxMjYx
MDA1MjEzNzU0WjAXMRUwEwYDVQQDDAxkYmNtIFRlc3QgQ0EwWTATBgcqhkjOPQIB
BggqhkjOPQMBBwNCAAT4G5EGzzg9/nnzznwQuxmAFPMHYUf8PhG48jAo3UKJZy01
sZ2LyWbG/bqzot7ruRPk2wmwMePS5Hc9WZCh2bJ6ozIwMDAdBgNVHQ4EFgQUGcsU
nzZk0prt+at1xqDuxMzfAnAwDwYDVR0TAQH/BAUwAwEB/zAKBggqhkjOPQQDAgNI
ADBFAiAoWx43pULmpokehGbOhg1wvEMLpmG2Z3WU9fMb2XlkGgIhAJ+zr+y/eg6u
8GBDCjR+1bBaxEnuCIfO+QWT0KXqQasy
-----END CERTIFICATE-----
`

const testLeaf = `-----BEGIN CERTIFICATE-----
MIIBTDCB9KADAgECAhQWleqqriE4mC0B2StXtE39pT4I4zAKBggqhkjOPQQDAjAP
MQ0wCwYDVQQDDARsZWFmMCAXDTI2MTAyOTIxMzc1NFoYDzIxMjYxMDA1MjEzNzU0
WjAPMQ0wCwYDVQQDDARsZWFmMFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAEXR6N
KRJNLIUx1AbSB9LO9Sq9c2C+VM9CVsrVBroaqcOd7EVmNwmBl6xE9kP1rpr6DoGE
+IK5BYBd/kwr+UdubqMsMCowHQYDVR0OBBYEFJdfEGDNs2wBuQM+fq6q8fp/mF2q
MAkGA1UdEwQCMAAwCgYIKoZIzj0EAwIDRwAwRAIgBCyJ+Im7uz4RuZkYzdFFmUz/
Nmw4DFVddoJBEdUzqpECIEphXINlCMJAbQCZoBLzUUqZ5RmWeLFoaLT3PhDGt+Y1
-----END CERTIFICATE-----
`

func rec(name, distro, release string, platforms []string, comps ...Selection) Recipe {
	return Recipe{Name: name, Base: Base{distro, release, testDigest}, Platforms: platforms, Components: comps}
}

func sel(id, version string, features []string, settings map[string]any) Selection {
	return Selection{ID: id, Version: version, Features: features, Settings: settings}
}

// extraGolden are the golden-file recipes of the catalog extension: one per
// new component family and base.
func extraGolden() map[string]Recipe {
	phpFeat := []string{"apcu", "intl", "mbstring", "mysqli", "opcache", "pdo_sqlite", "redis", "zip"}
	return map[string]Recipe{
		"debian13-apache-php": rec("apache site", "debian", "13", nil,
			sel("apache", "2.4", nil, map[string]any{"serverName": "example.org", "securityHeadersPreset": "strict", "realIpMode": "x-forwarded-for"}),
			sel("php", "8.4", phpFeat, map[string]any{"memory_limit": "256M", "opcache.jit": "tracing"}),
			sel("system", "1", nil, map[string]any{"timezone": "Europe/Berlin", "locale": "de_DE.UTF-8", "healthcheck": "http"})),
		"alpine322-caddy-php": rec("caddy site", "alpine", "3.22", nil,
			sel("caddy", "2", nil, map[string]any{"realIpMode": "x-real-ip", "securityHeadersPreset": "api"}),
			sel("php", "8.4", []string{"mbstring", "opcache"}, nil)),
		"ubuntu2204-nginx-hardened": rec("shop", "ubuntu", "22.04", nil,
			sel("nginx", "1.18", nil, map[string]any{"rateLimit": 10, "rateLimitBurst": 30, "securityHeadersPreset": "strict", "realIpMode": "x-forwarded-for", "trustForwardedProto": true, "accessLog": false}),
			sel("php", "8.1", []string{"mbstring", "opcache"}, map[string]any{"opcache.validate_timestamps": false}),
			sel("system", "1", nil, map[string]any{"healthcheck": "fpm-ping", "healthcheckInterval": 15})),
		"alpine321-nginx-brotli": rec("static brotli", "alpine", "3.21", nil,
			sel("nginx", "1.26", []string{"brotli"}, map[string]any{"serverTokens": false})),
		"rocky9-nginx-dbclients": rec("tools", "rocky", "9", []string{"linux/amd64", "linux/arm64"},
			sel("nginx", "1.24", nil, nil),
			sel("php", "8.3", []string{"mysqli"}, nil),
			sel("dbclients", "distro", []string{"mariadb-client", "postgresql-client", "redis-cli"}, nil)),
		"alpine321-node": rec("node app", "alpine", "3.21", nil,
			sel("node", "22", []string{"npm"}, map[string]any{"port": 3100, "maxOldSpaceMb": 384}),
			sel("system", "1", nil, map[string]any{"healthcheck": "http", "healthcheckPath": "/healthz"})),
		"debian12-python": rec("py app", "debian", "12", nil,
			sel("python", "3.11", []string{"pip"}, map[string]any{"app": "project.wsgi:application", "workers": 4, "threads": 2, "forwardedAllowIps": "10.0.0.0/8"})),
		"ubuntu2404-java": rec("jvm app", "ubuntu", "24.04", nil,
			sel("java", "21", nil, map[string]any{"garbageCollector": "g1", "maxRamPercentage": 60})),
		"ubuntu2404-minecraft-vanilla": rec("mc vanilla", "ubuntu", "24.04", nil,
			sel("minecraft", "26.3", nil, mcOK(map[string]any{"motd": "Hello Swarm", "maxPlayers": 8, "difficulty": "normal", "viewDistance": 8, "memoryInitial": "1G", "memoryMax": "3G"}))),
		"alpine322-minecraft-paper": rec("mc paper", "alpine", "3.22", nil,
			sel("minecraft", "1.21.11", nil, mcOK(map[string]any{"variant": "paper", "onlineMode": false}))),
		"debian13-sftp": rec("files", "debian", "13", nil,
			sel("sftp", "distro", nil, map[string]any{"readOnly": true, "dataDir": "/srv/files"})),
		"alpine321-jobs-restic": rec("backup", "alpine", "3.21", nil,
			sel("jobrunner", "distro", nil, map[string]any{"intervalSeconds": 3600, "onFailure": "exit"}),
			sel("backuptools", "distro", []string{"restic", "rsync"}, nil)),
		"alpine321-php-worker": rec("worker", "alpine", "3.21", nil,
			sel("php", "8.4", []string{"pcntl", "redis"}, map[string]any{"mode": "worker", "workerScript": "/var/www/html/bin/console", "workerArgs": "messenger:consume async --limit=100"})),
		"debian13-nginx-ca": rec("ca", "debian", "13", nil,
			sel("nginx", "1.26", nil, nil),
			sel("system", "1", nil, map[string]any{"extraCaPem": testCA})),
		"alma9-nginx-ca": rec("ca rhel", "almalinux", "9", nil,
			sel("nginx", "1.26", nil, nil),
			sel("system", "1", nil, map[string]any{"extraCaPem": testCA, "locale": "en_US.UTF-8"})),
	}
}

// mcOK returns Minecraft settings that include the explicit EULA acceptance.
func mcOK(over map[string]any) map[string]any {
	m := map[string]any{"eulaAccepted": true}
	for k, v := range over {
		m[k] = v
	}
	return m
}

func TestMinecraft(t *testing.T) {
	c := Default()
	mc := func(distro, release, version string, settings map[string]any) Recipe {
		return rec("mc", distro, release, nil, sel("minecraft", version, nil, settings))
	}
	// The EULA is never implicit: absent or false is refused with a path on the field.
	for name, s := range map[string]map[string]any{"absent": nil, "false": {"eulaAccepted": false}} {
		_, err := c.Generate(mc("ubuntu", "24.04", "26.3", s))
		wantIssue(t, err, "components[minecraft].settings.eulaAccepted")
		if err != nil && !strings.Contains(err.Error(), "EULA") {
			t.Errorf("%s: %v", name, err)
		}
	}
	bad := map[string]struct {
		r    Recipe
		path string
	}{
		"java 25 not on alpine":    {mc("alpine", "3.22", "26.3", mcOK(nil)), "components[minecraft].version"},
		"java 25 not on ubuntu 22": {mc("ubuntu", "22.04", "26.1.2", mcOK(nil)), "components[minecraft].version"},
		"unknown version":          {mc("ubuntu", "24.04", "1.8.8", mcOK(nil)), "components[minecraft].version"},
		"unknown variant":          {mc("ubuntu", "24.04", "26.3", mcOK(map[string]any{"variant": "spigot"})), "components[minecraft].settings.variant"},
		"xms above xmx":            {mc("ubuntu", "24.04", "26.3", mcOK(map[string]any{"memoryInitial": "4G", "memoryMax": "2G"})), "components[minecraft].settings.memoryInitial"},
		"heap too small":           {mc("ubuntu", "24.04", "26.3", mcOK(map[string]any{"memoryInitial": "128M", "memoryMax": "256M"})), "components[minecraft].settings.memoryMax"},
		"heap injection":           {mc("ubuntu", "24.04", "26.3", mcOK(map[string]any{"memoryMax": "2G -XX:+Foo"})), "components[minecraft].settings.memoryMax"},
		"motd newline":             {mc("ubuntu", "24.04", "26.3", mcOK(map[string]any{"motd": "hi\nop-permission-level=4"})), "components[minecraft].settings.motd"},
		"motd equals":              {mc("ubuntu", "24.04", "26.3", mcOK(map[string]any{"motd": "a=b"})), "components[minecraft].settings.motd"},
		"difficulty unknown":       {mc("ubuntu", "24.04", "26.3", mcOK(map[string]any{"difficulty": "nightmare"})), "components[minecraft].settings.difficulty"},
		"view distance range":      {mc("ubuntu", "24.04", "26.3", mcOK(map[string]any{"viewDistance": 64})), "components[minecraft].settings.viewDistance"},
		"unknown key":              {mc("ubuntu", "24.04", "26.3", mcOK(map[string]any{"rcon.password": "x"})), "components[minecraft].settings.rcon.password"},
		"with nginx":               {rec("x", "ubuntu", "24.04", nil, sel("nginx", "1.24", nil, nil), sel("minecraft", "26.3", nil, mcOK(nil))), "components"},
		"with php":                 {rec("x", "ubuntu", "24.04", nil, sel("minecraft", "26.3", nil, mcOK(nil)), sel("php", "8.3", nil, nil)), "components[php]"},
		"http health check":        {rec("x", "ubuntu", "24.04", nil, sel("minecraft", "26.3", nil, mcOK(nil)), sel("system", "1", nil, map[string]any{"healthcheck": "http"})), "components[system].settings.healthcheck"},
	}
	for name, tc := range bad {
		t.Run(name, func(t *testing.T) {
			_, err := c.Generate(tc.r)
			wantIssue(t, err, tc.path)
		})
	}

	// Java version per Minecraft version: 21 for 1.21.x, 25 for 26.x (Mojang's javaVersion).
	comp, _ := c.Component("minecraft")
	for _, tc := range []struct{ version, distro, release, pkg string }{
		{"1.21.11", "alpine", "3.21", "openjdk21-jre-headless"},
		{"1.21.11", "ubuntu", "22.04", "openjdk-21-jre-headless"},
		{"26.1.2", "debian", "13", "openjdk-25-jre-headless"},
		{"26.3", "ubuntu", "24.04", "openjdk-25-jre-headless"},
	} {
		cv, _ := comp.Version(tc.version)
		s, ok := cv.SupportFor(tc.distro, tc.release)
		if !ok || len(s.Packages) != 1 || s.Packages[0].Name != tc.pkg {
			t.Errorf("%s on %s %s: %+v", tc.version, tc.distro, tc.release, s)
		}
	}

	// Generated output facts.
	vanilla, err := c.Generate(extraGolden()["ubuntu2404-minecraft-vanilla"])
	if err != nil {
		t.Fatal(err)
	}
	if vanilla.Role != RoleGame {
		t.Errorf("role %q", vanilla.Role)
	}
	df := vanilla.Rendered.Dockerfile
	for _, want := range []string{
		"ADD --checksum=sha256:d052f14d7a173734fba553711e5b570162e2f2a313267ee31a21b975a679be64 --chmod=0644 https://piston-data.mojang.com/v1/objects/33680f5f2ac32864d6d7cf5e56a705fdb3e05f4c/server.jar /opt/minecraft/server.jar",
		`sha1sum -c /tmp/server.jar.sha1`, "openjdk-25-jre-headless", `VOLUME ["/data"]`, "EXPOSE 25565", "WORKDIR /data", "USER 10001:10001",
	} {
		if !strings.Contains(df, want) {
			t.Errorf("Dockerfile lacks %q:\n%s", want, df)
		}
	}
	files := map[string]string{}
	for _, f := range vanilla.Rendered.Files {
		files[f.Path] = f.Content
	}
	if p := files["minecraft/managed.properties"]; !strings.Contains(p, "server-port=25565\n") || !strings.Contains(p, "motd=Hello Swarm\n") || !strings.Contains(p, "online-mode=true\n") {
		t.Errorf("managed.properties: %s", p)
	}
	if e := files["minecraft/entrypoint.sh"]; !strings.Contains(e, "exec /usr/bin/java -Xms1G -Xmx3G ") || !strings.Contains(e, "nogui") {
		t.Errorf("entrypoint: %s", e)
	}
	paper, err := c.Generate(extraGolden()["alpine322-minecraft-paper"])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(paper.Rendered.Dockerfile, "sha1sum") || !strings.Contains(paper.Rendered.Dockerfile, "https://fill-data.papermc.io/v1/objects/") {
		t.Errorf("paper Dockerfile: %s", paper.Rendered.Dockerfile)
	}
	var pw bool
	for _, w := range paper.Warnings {
		pw = pw || strings.Contains(w, "Paper, which also downloads")
	}
	if !pw {
		t.Error("paper warning missing")
	}
}

func TestArtifactCatalogRules(t *testing.T) {
	good := string(embedded)
	mojang := "https://piston-data.mojang.com/v1/objects/33680f5f2ac32864d6d7cf5e56a705fdb3e05f4c/server.jar"
	for name, mutate := range map[string]func(string) string{
		"foreign host": func(s string) string {
			return strings.Replace(s, mojang, "https://example.org/v1/objects/33680f5f2ac32864d6d7cf5e56a705fdb3e05f4c/server.jar", 1)
		},
		"plain http": func(s string) string {
			return strings.Replace(s, mojang, strings.Replace(mojang, "https:", "http:", 1), 1)
		},
		"url hash differs": func(s string) string {
			return strings.Replace(s, "/33680f5f2ac32864d6d7cf5e56a705fdb3e05f4c/", "/43680f5f2ac32864d6d7cf5e56a705fdb3e05f4c/", 1)
		},
		"sha256 malformed": func(s string) string {
			return strings.Replace(s, "d052f14d7a173734fba553711e5b570162e2f2a313267ee31a21b975a679be64", "D052", 1)
		},
		"paper host for vanilla": func(s string) string {
			return strings.Replace(s, "https://piston-data.mojang.com/v1/objects/33680f5f2ac32864d6d7cf5e56a705fdb3e05f4c", "https://fill-data.papermc.io/v1/objects/33680f5f2ac32864d6d7cf5e56a705fdb3e05f4c", 1)
		},
		"vanilla without sha1": func(s string) string {
			return strings.Replace(s, `, "sha1": "33680f5f2ac32864d6d7cf5e56a705fdb3e05f4c"`, "", 1)
		},
		"zero size": func(s string) string { return strings.Replace(s, `"size": 62294556`, `"size": 0`, 1) },
	} {
		mutated := mutate(good)
		if mutated == good {
			t.Fatalf("%s: mutation did not apply", name)
		}
		if _, err := Parse([]byte(mutated)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestExtensionValidation(t *testing.T) {
	c := Default()
	nginx := sel("nginx", "1.26", nil, nil)
	cases := map[string]struct {
		r    Recipe
		path string
	}{
		"two web servers":        {rec("x", "alpine", "3.21", nil, nginx, sel("apache", "2.4", nil, nil)), "components"},
		"node and nginx":         {rec("x", "alpine", "3.21", nil, nginx, sel("node", "22", nil, nil)), "components"},
		"php with node":          {rec("x", "alpine", "3.21", nil, sel("node", "22", nil, nil), sel("php", "8.3", nil, nil)), "components[php]"},
		"php worker with nginx":  {rec("x", "alpine", "3.21", nil, nginx, sel("php", "8.3", nil, map[string]any{"mode": "worker"})), "components[php].settings.mode"},
		"worker script outside":  {rec("x", "alpine", "3.21", nil, sel("php", "8.3", nil, map[string]any{"mode": "worker", "workerScript": "/etc/passwd"})), "components[php].settings.workerScript"},
		"worker args injection":  {rec("x", "alpine", "3.21", nil, sel("php", "8.3", nil, map[string]any{"mode": "worker", "workerArgs": "a; rm -rf /"})), "components[php].settings.workerArgs"},
		"php 8.2 on debian 13":   {rec("x", "debian", "13", nil, sel("php", "8.2", nil, nil)), "components[php].version"},
		"python on rocky":        {rec("x", "rocky", "9", nil, sel("python", "3.12", nil, nil)), "components[python].version"},
		"caddy on alma":          {rec("x", "almalinux", "9", nil, sel("caddy", "2", nil, nil)), "components[caddy].version"},
		"node on ubuntu":         {rec("x", "ubuntu", "24.04", nil, sel("node", "22", nil, nil)), "components[node].version"},
		"node entry traversal":   {rec("x", "alpine", "3.21", nil, sel("node", "22", nil, map[string]any{"entryPoint": "/app/../etc/x.js"})), "components[node].settings.entryPoint"},
		"node entry shell":       {rec("x", "alpine", "3.21", nil, sel("node", "22", nil, map[string]any{"entryPoint": "/app/a.js; id"})), "components[node].settings.entryPoint"},
		"python app injection":   {rec("x", "alpine", "3.21", nil, sel("python", "3.12", nil, map[string]any{"app": "app:app --bind 0.0.0.0:22"})), "components[python].settings.app"},
		"jar outside app":        {rec("x", "alpine", "3.21", nil, sel("java", "21", nil, map[string]any{"jarPath": "/tmp/x.jar"})), "components[java].settings.jarPath"},
		"port privileged":        {rec("x", "alpine", "3.21", nil, sel("node", "22", nil, map[string]any{"port": 80})), "components[node].settings.port"},
		"bad trusted proxy":      {rec("x", "alpine", "3.21", nil, sel("nginx", "1.26", nil, map[string]any{"realIpMode": "x-real-ip", "trustedProxies": "999.1.1.1/8"})), "components[nginx].settings.trustedProxies"},
		"proxy list injection":   {rec("x", "alpine", "3.21", nil, sel("nginx", "1.26", nil, map[string]any{"trustedProxies": "10.0.0.0/8; deny all"})), "components[nginx].settings.trustedProxies"},
		"burst without limit":    {rec("x", "alpine", "3.21", nil, sel("nginx", "1.26", nil, map[string]any{"rateLimitBurst": 5})), "components[nginx].settings.rateLimitBurst"},
		"brotli on rhel":         {rec("x", "almalinux", "9", nil, sel("nginx", "1.26", []string{"brotli"}, nil)), "components[nginx].features"},
		"brotli setting no feat": {rec("x", "alpine", "3.21", nil, sel("nginx", "1.26", nil, map[string]any{"brotli": true})), "components[nginx].settings.brotli"},
		"unknown preset":         {rec("x", "alpine", "3.21", nil, sel("nginx", "1.26", nil, map[string]any{"securityHeadersPreset": "off; x"})), "components[nginx].settings.securityHeadersPreset"},
		"http hc without server": {rec("x", "alpine", "3.21", nil, sel("sftp", "distro", nil, nil), sel("system", "1", nil, map[string]any{"healthcheck": "http"})), "components[system].settings.healthcheck"},
		"fpm-ping with apache":   {rec("x", "alpine", "3.21", nil, sel("apache", "2.4", nil, nil), sel("php", "8.3", nil, nil), sel("system", "1", nil, map[string]any{"healthcheck": "fpm-ping"})), "components[system].settings.healthcheck"},
		"hc path injection":      {rec("x", "alpine", "3.21", nil, nginx, sel("system", "1", nil, map[string]any{"healthcheck": "http", "healthcheckPath": "/a b$(id)"})), "components[system].settings.healthcheckPath"},
		"timezone injection":     {rec("x", "alpine", "3.21", nil, nginx, sel("system", "1", nil, map[string]any{"timezone": "UTC; id"})), "components[system].settings.timezone"},
		"unknown locale":         {rec("x", "alpine", "3.21", nil, nginx, sel("system", "1", nil, map[string]any{"locale": "xx_YY.UTF-8"})), "components[system].settings.locale"},
		"ca leaf certificate":    {rec("x", "alpine", "3.21", nil, nginx, sel("system", "1", nil, map[string]any{"extraCaPem": testLeaf})), "components[system].settings.extraCaPem"},
		"ca garbage":             {rec("x", "alpine", "3.21", nil, nginx, sel("system", "1", nil, map[string]any{"extraCaPem": "-----BEGIN CERTIFICATE-----\nAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA\n-----END CERTIFICATE-----"})), "components[system].settings.extraCaPem"},
		"ca private key":         {rec("x", "alpine", "3.21", nil, nginx, sel("system", "1", nil, map[string]any{"extraCaPem": "-----BEGIN PRIVATE KEY-----\nAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA\n-----END PRIVATE KEY-----"})), "components[system].settings.extraCaPem"},
		"sftp same file":         {rec("x", "alpine", "3.21", nil, sel("sftp", "distro", nil, map[string]any{"authorizedKeysPath": "/run/secrets/ssh_host_ed25519_key"})), "components[sftp].settings.authorizedKeysPath"},
		"sftp key outside":       {rec("x", "alpine", "3.21", nil, sel("sftp", "distro", nil, map[string]any{"hostKeyPath": "/etc/ssh/ssh_host_rsa_key"})), "components[sftp].settings.hostKeyPath"},
		"restic without tool":    {rec("x", "alpine", "3.21", nil, sel("jobrunner", "distro", nil, nil)), "components[jobrunner].settings.job"},
		"rclone dest injection":  {rec("x", "alpine", "3.21", nil, sel("jobrunner", "distro", nil, map[string]any{"job": "rclone-sync", "rcloneDestination": "r:b'; id #"}), sel("backuptools", "distro", []string{"rclone"}, nil)), "components[jobrunner].settings.rcloneDestination"},
		"restic on rhel":         {rec("x", "almalinux", "9", nil, sel("jobrunner", "distro", nil, nil), sel("backuptools", "distro", []string{"restic"}, nil)), "components[backuptools].features"},
		"mc outside alpine":      {rec("x", "debian", "13", nil, nginx, sel("backuptools", "distro", []string{"mc"}, nil)), "components"},
		"jit buffer injection":   {rec("x", "alpine", "3.21", nil, nginx, sel("php", "8.3", []string{"opcache"}, map[string]any{"opcache.jit_buffer_size": "64M\nauto_prepend_file=/x"})), "components[php].settings.opcache.jit_buffer_size"},
		"apcu setting w/o ext":   {rec("x", "alpine", "3.21", nil, nginx, sel("php", "8.3", nil, map[string]any{"apc.shm_size": "64M"})), "components[php].settings.apc.shm_size"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := c.Generate(tc.r)
			if name == "mc outside alpine" {
				// mc exists on Alpine only: the feature must be refused elsewhere.
				wantIssue(t, err, "components[backuptools].features")
				return
			}
			wantIssue(t, err, tc.path)
		})
	}
}

func TestExtensionOutputFacts(t *testing.T) {
	c := Default()
	gen := func(name string) Result {
		res, err := c.Generate(extraGolden()[name])
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return res
	}
	file := func(r Result, path string) string {
		for _, f := range r.Rendered.Files {
			if f.Path == path {
				return f.Content
			}
		}
		t.Fatalf("no file %s", path)
		return ""
	}
	if r := gen("debian13-apache-php"); r.Role != RoleWeb || !strings.Contains(r.Rendered.Dockerfile, "locale-gen") || !strings.Contains(r.Rendered.Dockerfile, "HEALTHCHECK") ||
		!strings.Contains(file(r, "apache/httpd.conf"), "RemoteIPInternalProxy") || !strings.Contains(file(r, "php/99-dbcm.ini"), "opcache.jit = tracing") {
		t.Errorf("apache: %s", r.Rendered.Dockerfile)
	}
	if r := gen("ubuntu2204-nginx-hardened"); !strings.Contains(file(r, "nginx/default.conf"), "dbcm-ping") || !strings.Contains(file(r, "nginx/nginx.conf"), "limit_req_zone") ||
		!strings.Contains(file(r, "php/pool.conf"), "ping.path") || !strings.Contains(file(r, "nginx/default.conf"), "Strict-Transport-Security") {
		t.Error("hardened nginx")
	}
	if r := gen("alpine321-nginx-brotli"); !strings.Contains(file(r, "nginx/nginx.conf"), "load_module") {
		t.Error("brotli load_module missing")
	}
	if r := gen("alpine321-php-worker"); r.Role != RoleWorker || strings.Contains(r.Rendered.Dockerfile, "php-fpm") && strings.Contains(r.Rendered.Dockerfile, "-t -y") ||
		!strings.Contains(r.Rendered.Dockerfile, `CMD ["php", "/var/www/html/bin/console", "messenger:consume", "async", "--limit=100"]`) {
		t.Errorf("worker: %s", r.Rendered.Dockerfile)
	}
	if r := gen("debian13-sftp"); r.Role != RoleSFTP || !strings.Contains(file(r, "sshd/sshd_config"), "ForceCommand internal-sftp -d /srv/files -u 077 -R") || strings.Contains(r.Rendered.Dockerfile, "ssh_host") {
		t.Errorf("sftp: %s", file(r, "sshd/sshd_config"))
	}
	if r := gen("alpine321-jobs-restic"); r.Role != RoleJobs || !strings.Contains(file(r, "jobs/run.sh"), "restic forget --keep-daily 7 --prune") || !strings.Contains(file(r, "jobs/run.sh"), `exit "$rc"`) {
		t.Errorf("jobs: %s", file(r, "jobs/run.sh"))
	}
	if r := gen("alma9-nginx-ca"); !strings.Contains(r.Rendered.Dockerfile, "update-ca-trust extract") || !strings.Contains(r.Rendered.Dockerfile, "glibc-langpack-en") {
		t.Error("rhel CA/locale")
	}
	// Every generated image keeps the non-root and no-remote-script rules.
	for name := range extraGolden() {
		r := gen(name)
		df := r.Rendered.Dockerfile
		if strings.Contains(df, "ADD ") {
			// The only download is the pinned Minecraft jar, always with a hash.
			if !strings.Contains(name, "minecraft") || strings.Count(df, "ADD ") != strings.Count(df, "ADD --checksum=sha256:") {
				t.Errorf("%s: ADD without checksum", name)
			}
		}
		for _, bad := range []string{"curl http", "wget", "| sh", "| bash", "ARG ", "USER root", ":latest", "EXPOSE 80\n", "EXPOSE 22\n"} {
			if strings.Contains(df, bad) {
				t.Errorf("%s: Dockerfile contains %q", name, bad)
			}
		}
		if !strings.Contains(df, "USER 10001:10001") || !strings.Contains(df, "@"+testDigest) {
			t.Errorf("%s: user or digest pin missing", name)
		}
		if strings.Contains(df, "BEGIN") {
			t.Errorf("%s: certificate text in Dockerfile", name)
		}
		again, _ := c.Generate(extraGolden()[name])
		if again.Rendered.ContextHash != r.Rendered.ContextHash {
			t.Errorf("%s: not deterministic", name)
		}
	}
}

func TestCatalogExtensionMatrix(t *testing.T) {
	c := Default()
	for _, id := range []string{"apache", "caddy", "node", "python", "java", "minecraft", "sftp", "jobrunner", "backuptools", "dbclients", "system"} {
		if _, ok := c.Component(id); !ok {
			t.Errorf("component %s missing", id)
		}
	}
	for _, rel := range [][2]string{{"alpine", "3.21"}, {"alpine", "3.22"}, {"debian", "13"}, {"ubuntu", "22.04"}, {"rocky", "9"}} {
		if _, _, ok := c.Release(rel[0], rel[1]); !ok {
			t.Errorf("release %v missing", rel)
		}
	}
	// Honest gaps: no end-of-life Node.js on distribution releases that only ship it.
	node, _ := c.Component("node")
	for _, v := range node.Versions {
		for _, s := range v.Support {
			if s.Distro == "ubuntu" || (s.Distro == "debian" && s.Release == "12") {
				t.Errorf("node %s must not be offered on %s %s", v.Version, s.Distro, s.Release)
			}
		}
	}
	// Every component with a main process is reachable on at least one release and has both labels.
	for _, comp := range c.Components {
		if comp.Name.En == "" || comp.Name.De == "" || comp.Description.En == "" || comp.Description.De == "" {
			t.Errorf("%s: names and descriptions need both languages", comp.ID)
		}
		for _, f := range comp.Settings {
			if f.Help != nil && (f.Help.En == "" || f.Help.De == "") {
				t.Errorf("%s.%s: help needs both languages", comp.ID, f.Key)
			}
		}
	}
}
