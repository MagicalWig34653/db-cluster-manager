// SPDX-License-Identifier: AGPL-3.0-only

package imagecompose

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/MagicalWig34653/db-cluster-manager/internal/imagebuild"
)

// Roles of a generated image, derived from the selection.
const (
	RoleWeb    = "web"    // a web server (nginx, apache, caddy), with PHP-FPM as companion when PHP is selected
	RoleFPM    = "fpm"    // PHP-FPM alone as the single main process
	RoleWorker = "worker" // a PHP CLI worker script as the single main process
	RoleApp    = "app"    // node, python (gunicorn) or java
	RoleGame   = "game"   // Minecraft Java Edition server
	RoleSFTP   = "sftp"   // OpenSSH serving SFTP only
	RoleJobs   = "jobs"   // scheduled job runner
)

// PackageRef is one distribution package of the image, the input of the
// SBOM and vulnerability inventory (X13).
type PackageRef struct {
	Component  string `json:"component"`
	Name       string `json:"name"`
	Constraint string `json:"constraint,omitempty"`
}

// Result is the reviewed output for one recipe: the normalized recipe, its
// digest, the generated context and the facts a reviewer needs.
type Result struct {
	Recipe   Recipe              `json:"recipe"`
	Digest   string              `json:"digest"`
	Role     string              `json:"role"`
	Rendered imagebuild.Rendered `json:"rendered"`
	Packages []PackageRef        `json:"packages"`
	Modules  []string            `json:"modules,omitempty"`
	// Warnings are limits to know before approving; Notes describe the
	// hardening defaults and reproducibility properties of this output.
	Warnings []string `json:"warnings"`
	Notes    []string `json:"notes"`
}

const (
	appUID   = 10001
	webPort  = 8080
	fpmPort  = 9000
	confRoot = "/etc/dbcm"
	pingPath = "/dbcm-ping"
)

var (
	webServerIDs = []string{"nginx", "apache", "caddy"}
	appIDs       = []string{"node", "python", "java"}
)

type pkgLine struct {
	ref      PackageRef
	rendered string
}

// build collects everything the Dockerfile and context need, in the order
// the components are processed (a fixed order), so output stays deterministic.
type build struct {
	family    string
	sel       map[string]Selection
	sup       map[string]Support
	role      string
	web       string // selected web server id, if any
	files     []imagebuild.File
	writable  []string // directories created and owned by the app user
	readonly  []string // directories created, root-owned
	preUser   []string // Dockerfile lines (COPY/RUN as root) after the package install
	copies    []string // Dockerfile COPY lines
	env       map[string]string
	checks    []string // RUN lines that test the final configuration
	expose    int
	exposeUDP int // optional second, UDP port
	workdir   string
	runtime   []string // STOPSIGNAL and ENTRYPOINT/CMD
	health    string
	packages  map[string]pkgLine
	modules   map[string]bool
}

// Generate validates and renders a recipe. It is a pure function: the same
// recipe, catalog and generator version always give byte-identical output;
// no clock, randomness, network or registry lookup is involved.
func (c *Catalog) Generate(r Recipe) (Result, error) {
	n, err := c.Resolve(r)
	if err != nil {
		return Result{}, err
	}
	rel, family, _ := c.Release(n.Base.Distro, n.Base.Release)
	res := Result{Recipe: n, Digest: c.Digest(n)}
	baseRef := rel.Image + ":" + rel.Tag + "@" + n.Base.Digest

	b := &build{family: family, sel: map[string]Selection{}, sup: map[string]Support{}, env: map[string]string{"HOME": "/tmp"}, packages: map[string]pkgLine{}, modules: map[string]bool{}}
	for _, s := range n.Components {
		b.sel[s.ID] = s
		comp, _ := c.Component(s.ID)
		cv, _ := comp.Version(s.Version)
		b.sup[s.ID], _ = cv.SupportFor(n.Base.Distro, n.Base.Release)
	}
	b.role = roleOf(n)
	for _, id := range webServerIDs {
		if _, ok := b.sel[id]; ok {
			b.web = id
		}
	}
	res.Role = b.role

	// Packages and dnf modules from the matrix and the selected features.
	for _, s := range n.Components {
		comp, _ := c.Component(s.ID)
		su := b.sup[s.ID]
		for _, m := range su.Modules {
			b.modules[m] = true
		}
		for _, p := range su.Packages {
			add(b.packages, family, s.ID, p.Name, p.Constraint)
		}
		for _, f := range s.Features {
			feat, _ := comp.Feature(f)
			for _, tpl := range feat.Packages[family] {
				add(b.packages, family, s.ID, strings.ReplaceAll(tpl, "{php}", b.sup["php"].Prefix), "")
			}
		}
	}

	// Renderers, in a fixed order.
	switch b.web {
	case "nginx":
		b.nginx()
	case "apache":
		b.apache()
	case "caddy":
		b.caddy()
	}
	if _, ok := b.sel["php"]; ok {
		c.php(b)
	}
	switch {
	case b.sel["node"].ID != "":
		b.node()
	case b.sel["python"].ID != "":
		b.python()
	case b.sel["java"].ID != "":
		b.java()
	case b.sel["minecraft"].ID != "":
		c.minecraft(b)
	case b.sel["sftp"].ID != "":
		b.sftp()
	case b.sel["jobrunner"].ID != "":
		b.jobs()
	}
	if _, ok := b.sel["system"]; ok {
		b.system()
	}
	b.entrypoint()

	keys := make([]string, 0, len(b.packages))
	for k := range b.packages {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		res.Packages = append(res.Packages, b.packages[k].ref)
	}
	for m := range b.modules {
		res.Modules = append(res.Modules, m)
	}
	sort.Strings(res.Modules)
	sort.Slice(b.files, func(i, j int) bool { return b.files[i].Path < b.files[j].Path })

	// Dockerfile.
	var d strings.Builder
	d.WriteString("# syntax=docker/dockerfile:1\n")
	d.WriteString("# Generated by cluster-manager (imagecompose). Review before building.\n")
	fmt.Fprintf(&d, "FROM %s\n", baseRef)
	fmt.Fprintf(&d, "LABEL org.opencontainers.image.title=%q \\\n      org.opencontainers.image.base.name=%q \\\n      org.opencontainers.image.base.digest=%q \\\n      dbcm.recipe-digest=%q \\\n      dbcm.catalog-version=%q \\\n      dbcm.generator-version=%q\n",
		n.Name, rel.Image+":"+rel.Tag, n.Base.Digest, res.Digest, c.Version, GeneratorVersion)
	d.WriteString(installLine(family, res.Modules, keys))
	if su := b.sup["php"]; b.sel["php"].ID != "" && su.CLI != "" {
		fmt.Fprintf(&d, "RUN ln -sf /usr/bin/%s /usr/local/bin/php\n", su.CLI)
	}
	for _, l := range b.preUser {
		d.WriteString(l + "\n")
	}
	d.WriteString(userLine(family, b.writable, b.readonly))
	for _, l := range b.copies {
		d.WriteString(l + "\n")
	}
	envKeys := make([]string, 0, len(b.env))
	for k := range b.env {
		envKeys = append(envKeys, k)
	}
	sort.Strings(envKeys)
	for _, k := range envKeys {
		fmt.Fprintf(&d, "ENV %s=%s\n", k, b.env[k])
	}
	for _, l := range b.checks {
		d.WriteString(l + "\n")
	}
	fmt.Fprintf(&d, "USER %d:%d\n", appUID, appUID)
	if b.workdir != "" {
		fmt.Fprintf(&d, "WORKDIR %s\n", b.workdir)
	}
	if b.expose > 0 {
		fmt.Fprintf(&d, "EXPOSE %d\n", b.expose)
	}
	if b.exposeUDP > 0 {
		fmt.Fprintf(&d, "EXPOSE %d/udp\n", b.exposeUDP)
	}
	if b.health != "" {
		d.WriteString(b.health + "\n")
	}
	for _, l := range b.runtime {
		d.WriteString(l + "\n")
	}

	res.Rendered = imagebuild.Rendered{SpecHash: res.Digest, Dockerfile: d.String(), Files: b.files, BaseImage: baseRef}
	res.Rendered.ContextHash = imagebuild.ComputeContextHash(res.Rendered)
	res.Warnings, res.Notes = c.advice(n, rel, family, b)
	return res, nil
}

// roleOf derives the image role from a normalized or raw recipe.
func roleOf(n Recipe) string {
	has := map[string]Selection{}
	for _, s := range n.Components {
		has[s.ID] = s
	}
	for _, id := range webServerIDs {
		if _, ok := has[id]; ok {
			return RoleWeb
		}
	}
	for _, id := range appIDs {
		if _, ok := has[id]; ok {
			return RoleApp
		}
	}
	if _, ok := has["minecraft"]; ok {
		return RoleGame
	}
	if _, ok := has["sftp"]; ok {
		return RoleSFTP
	}
	if _, ok := has["jobrunner"]; ok {
		return RoleJobs
	}
	if p, ok := has["php"]; ok {
		if m, _ := p.Settings["mode"].(string); m == "worker" {
			return RoleWorker
		}
		return RoleFPM
	}
	return ""
}

// httpPort is the port a plain HTTP health check can use, or 0.
func httpPort(n Recipe) int {
	has := map[string]Selection{}
	for _, s := range n.Components {
		has[s.ID] = s
	}
	for _, id := range webServerIDs {
		if _, ok := has[id]; ok {
			return webPort
		}
	}
	for _, id := range []string{"node", "python", "java"} {
		if s, ok := has[id]; ok {
			p, _ := s.Settings["port"].(int64)
			return int(p)
		}
	}
	return 0
}

// add renders one package with the family's version-constraint syntax.
func add(m map[string]pkgLine, family, comp, name, constraint string) {
	r := name
	if constraint != "" {
		switch family {
		case "alpine":
			r = name + "~=" + constraint
		case "debian":
			r = name + "=" + constraint + ".*"
		}
	}
	if _, dup := m[r]; !dup {
		ref := PackageRef{Component: comp, Name: name}
		if constraint != "" && family != "rhel" {
			ref.Constraint = constraint
		}
		m[r] = pkgLine{ref: ref, rendered: r}
	}
}

func list(prefix string, items []string) string {
	var b strings.Builder
	for i, it := range items {
		b.WriteString(prefix + it)
		if i < len(items)-1 {
			b.WriteString(" \\\n")
		}
	}
	return b.String()
}

func installLine(family string, modules, pkgs []string) string {
	if len(pkgs) == 0 {
		return ""
	}
	switch family {
	case "alpine":
		return "RUN apk add --no-cache \\\n" + list("      ", pkgs) + "\n"
	case "debian":
		return "RUN apt-get update \\\n && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \\\n" + list("      ", pkgs) + " \\\n && rm -rf /var/lib/apt/lists/*\n"
	default:
		s := "RUN "
		if len(modules) > 0 {
			s += "dnf -y module enable " + strings.Join(modules, " ") + " \\\n && "
		}
		return s + "dnf -y install --setopt=install_weak_deps=False --setopt=tsflags=nodocs \\\n" + list("      ", pkgs) + " \\\n && dnf clean all && rm -rf /var/cache/dnf\n"
	}
}

func userLine(family string, writable, readonly []string) string {
	var s string
	switch family {
	case "alpine":
		s = fmt.Sprintf("RUN addgroup -g %d -S app && adduser -S -D -H -u %d -G app -s /sbin/nologin app", appUID, appUID)
	default:
		s = fmt.Sprintf("RUN groupadd --system --gid %d app && useradd --system --uid %d --gid app --no-create-home --shell /usr/sbin/nologin app", appUID, appUID)
	}
	w, ro := uniqSorted(writable), uniqSorted(readonly)
	all := append(append([]string{}, w...), ro...)
	if len(all) == 0 {
		return s + "\n"
	}
	s += " \\\n && mkdir -p " + strings.Join(all, " ")
	if len(w) > 0 {
		s += " \\\n && chown -R app:app " + strings.Join(w, " ")
	}
	return s + "\n"
}

func uniqSorted(in []string) []string {
	m := map[string]bool{}
	var out []string
	for _, s := range in {
		if !m[s] {
			m[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

// arr renders a JSON exec-form array for CMD, ENTRYPOINT and HEALTHCHECK.
func arr(items ...string) string {
	parts := make([]string, len(items))
	for i, it := range items {
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)
		_ = enc.Encode(it)
		parts[i] = strings.TrimSpace(buf.String())
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

func gs(o map[string]any, k string) string { v, _ := o[k].(string); return v }
func gi(o map[string]any, k string) int64  { v, _ := o[k].(int64); return v }
func gb(o map[string]any, k string) bool   { v, _ := o[k].(bool); return v }

func b2(v any) string {
	if b, _ := v.(bool); b {
		return "on"
	}
	return "off"
}

func yn(v any) string {
	if b, _ := v.(bool); b {
		return "yes"
	}
	return "no"
}

func iniVal(v any) string {
	switch x := v.(type) {
	case bool:
		if x {
			return "On"
		}
		return "Off"
	default:
		return fmt.Sprint(x)
	}
}

// entrypoint decides how the container starts. With PHP-FPM as companion a
// generated entrypoint backgrounds PHP-FPM and replaces itself with the web
// server; there is no process supervisor.
func (b *build) entrypoint() {
	phpBin := b.sup["php"].Binary
	_, hasPHP := b.sel["php"]
	if b.role != RoleWeb || !hasPHP {
		return
	}
	cmd := b.webCommand()
	sh := "#!/bin/sh\n# Generated by cluster-manager (imagecompose).\n# PHP-FPM runs as a companion of the web server; there is no process supervisor.\nset -eu\n" +
		phpBin + " -F -y " + confRoot + "/php/php-fpm.conf &\nexec " + cmd + "\n"
	b.files = append(b.files, imagebuild.File{Path: "entrypoint.sh", Mode: 0o755, Content: sh})
	b.copies = append(b.copies, "COPY entrypoint.sh /usr/local/bin/dbcm-entrypoint")
	b.runtime = append(b.runtime, "ENTRYPOINT "+arr("/usr/local/bin/dbcm-entrypoint"))
}

// webCommand is the shell-safe foreground command line of the web server.
func (b *build) webCommand() string {
	switch b.web {
	case "apache":
		return b.sup["apache"].Binary + " -D FOREGROUND -f " + confRoot + "/apache/httpd.conf"
	case "caddy":
		return b.sup["caddy"].Binary + " run --config " + confRoot + "/caddy/Caddyfile --adapter caddyfile"
	default:
		return "nginx -g 'daemon off;' -c " + confRoot + "/nginx/nginx.conf"
	}
}

func (b *build) webCmdArray() []string {
	switch b.web {
	case "apache":
		return []string{b.sup["apache"].Binary, "-D", "FOREGROUND", "-f", confRoot + "/apache/httpd.conf"}
	case "caddy":
		return []string{b.sup["caddy"].Binary, "run", "--config", confRoot + "/caddy/Caddyfile", "--adapter", "caddyfile"}
	default:
		return []string{"nginx", "-g", "daemon off;", "-c", confRoot + "/nginx/nginx.conf"}
	}
}

func (c *Catalog) renderINI(p Selection) string {
	comp, _ := c.Component("php")
	var b strings.Builder
	b.WriteString("; Generated by cluster-manager (imagecompose). Change the recipe, not this file.\n")
	b.WriteString("; Fixed hardening lines (not configurable):\nallow_url_include = Off\ncgi.fix_pathinfo = 0\nlog_errors = On\nerror_log = /proc/self/fd/2\nsession.save_path = /tmp\nupload_tmp_dir = /tmp\n")
	b.WriteString("; Settings from the recipe:\n")
	for _, f := range comp.Settings {
		if f.Target != "ini" {
			continue
		}
		if v, ok := p.Settings[f.Key]; ok {
			fmt.Fprintf(&b, "%s = %s\n", f.Key, iniVal(v))
		}
	}
	return b.String()
}

func (c *Catalog) renderPool(p Selection, listenAll, ping bool) string {
	s := p.Settings
	listen := fmt.Sprintf("127.0.0.1:%d", fpmPort)
	if listenAll {
		listen = fmt.Sprintf("0.0.0.0:%d", fpmPort)
	}
	var b strings.Builder
	b.WriteString("; Generated by cluster-manager (imagecompose). Change the recipe, not this file.\n[www]\n")
	fmt.Fprintf(&b, "listen = %s\nclear_env = yes\ncatch_workers_output = %s\n", listen, yn(s["catch_workers_output"]))
	pm, _ := s["pm"].(string)
	fmt.Fprintf(&b, "pm = %s\npm.max_children = %v\npm.max_requests = %v\nrequest_terminate_timeout = %vs\n", pm, s["pm.max_children"], s["pm.max_requests"], s["request_terminate_timeout"])
	switch pm {
	case "dynamic":
		fmt.Fprintf(&b, "pm.start_servers = %v\npm.min_spare_servers = %v\npm.max_spare_servers = %v\n", s["pm.start_servers"], s["pm.min_spare_servers"], s["pm.max_spare_servers"])
	case "ondemand":
		fmt.Fprintf(&b, "pm.process_idle_timeout = %vs\n", s["pm.process_idle_timeout"])
	}
	if ping {
		fmt.Fprintf(&b, "ping.path = %s\nping.response = pong\n", pingPath)
	}
	return b.String()
}

// php renders the PHP configuration and its checks.
func (c *Catalog) php(b *build) {
	p := b.sel["php"]
	phpBin := b.sup["php"].Binary
	b.readonly = append(b.readonly, confRoot+"/php/conf.d")
	b.files = append(b.files, imagebuild.File{Path: "php/99-dbcm.ini", Mode: 0o644, Content: c.renderINI(p)})
	b.copies = append(b.copies, fmt.Sprintf("COPY php/99-dbcm.ini %s/php/conf.d/99-dbcm.ini", confRoot))
	b.env["PHP_INI_SCAN_DIR"] = ":" + confRoot + "/php/conf.d"
	if b.role == RoleWorker {
		b.runtime = append(b.runtime, "CMD "+arr(append([]string{"php", gs(p.Settings, "workerScript")}, strings.Fields(gs(p.Settings, "workerArgs"))...)...))
		b.writable = append(b.writable, "/var/www/html")
		return
	}
	hc := gs(b.sel["system"].Settings, "healthcheck")
	ping := hc == "fpm-ping"
	b.readonly = append(b.readonly, confRoot+"/php/pool.d")
	b.files = append(b.files,
		imagebuild.File{Path: "php/php-fpm.conf", Mode: 0o644, Content: "; Generated by cluster-manager (imagecompose). Change the recipe, not this file.\n[global]\npid = /tmp/php-fpm.pid\nerror_log = /proc/self/fd/2\ndaemonize = no\ninclude = " + confRoot + "/php/pool.d/*.conf\n"},
		imagebuild.File{Path: "php/pool.conf", Mode: 0o644, Content: c.renderPool(p, b.role == RoleFPM, ping)},
	)
	b.copies = append(b.copies,
		fmt.Sprintf("COPY php/php-fpm.conf %s/php/php-fpm.conf", confRoot),
		fmt.Sprintf("COPY php/pool.conf %s/php/pool.d/www.conf", confRoot))
	b.checks = append(b.checks, fmt.Sprintf("RUN %s -R -t -y %s/php/php-fpm.conf", phpBin, confRoot))
	if b.role == RoleFPM {
		b.writable = append(b.writable, "/var/www/html")
		b.expose = fpmPort
		b.runtime = append(b.runtime, "CMD "+arr(phpBin, "-F", "-y", confRoot+"/php/php-fpm.conf"))
	}
}

func (c *Catalog) advice(n Recipe, rel Release, family string, b *build) (warnings, notes []string) {
	sel := b.sel
	warnings = []string{
		"Only the base image is pinned by digest. Distribution packages are constrained to the selected major.minor version but their patch versions float: two builds on different days can differ.",
		"The component catalog is marked unverified: package names, versions and file locations were not checked against live repositories. The build itself is the check and fails on a wrong name; configuration tests run at build time where the tool allows it.",
		"The build service does not generate an SBOM or a vulnerability scan yet (placeholder); the package list of this preview is the intended input.",
	}
	if b.role == RoleWeb {
		if _, ok := sel["php"]; ok {
			warnings = append(warnings, "The web server and PHP-FPM run in one container without a process supervisor: PHP-FPM is started in the background by the entrypoint, the web server replaces the shell as the main process. If PHP-FPM dies, the web server keeps running and answers 502; use the health check (preset fpm-ping with nginx) or the service health check.")
		}
	}
	switch b.role {
	case RoleFPM:
		warnings = append(warnings, "PHP-FPM listens on 0.0.0.0:9000 and speaks unauthenticated FastCGI: expose it only on an isolated service network.")
	case RoleWorker:
		warnings = append(warnings, "The PHP worker script and its application code are not part of the image: provide them by volume or a derived image. If the script exits, the container exits and Swarm restarts it.")
	case RoleApp:
		warnings = append(warnings, "The application is not part of the image: mount it at /app or build a derived image from this one. The listen port is informational; the application itself must bind it.")
	case RoleGame:
		art := "the Mojang server"
		if gs(sel["minecraft"].Settings, "variant") == "paper" {
			art = "Paper, which also downloads the Mojang server files itself on first start (outbound access to Mojang download hosts is needed once; the files are then kept in /data/cache)"
		}
		warnings = append(warnings,
			"Minecraft: the image contains "+art+". The EULA acceptance in the recipe is yours; nothing accepts it on your behalf.",
			"Mount a volume at /data that user 10001 can write (world, server.properties, logs). Only the recipe's server.properties keys (port, motd, max-players, difficulty, view-distance, online-mode) are rewritten on every start; other keys are kept.",
			"Give the Swarm service a memory limit at least 512M above the maximum heap and a stop grace period of 60 s or more so the world is saved on shutdown. There is no health check preset for the game port yet (a status ping is not an HTTP check).",
			"Port 25565 is raw TCP: publish it in host mode or through a TCP-capable proxy, not through an HTTP router.")
		if gb(sel["minecraft"].Settings, "crossplay") {
			warnings = append(warnings,
				"Crossplay: Geyser listens on UDP 19132 for Bedrock players; publish it as UDP (host mode or an UDP-capable proxy). Geyser and Floodgate are copied from the image to /data/plugins on every start; their configuration and the Floodgate key stay in the volume. Geyser downloads Mojang's client files itself on first start (outbound access needed once). Switching crossplay off does not delete the jars already in /data/plugins.")
		}
	case RoleSFTP:
		warnings = append(warnings, "SFTP runs without root: one account (app), no chroot, no password login. The host key and authorized keys must be mounted as secrets; the container does not start without them. sshd as a non-root user is unverified in a container.")
	case RoleJobs:
		warnings = append(warnings, "Credentials (repository, password file, rclone configuration) are not part of the image: provide them as service environment or secrets. The job loop is a generated shell script; the tools must be selected in Backup and sync tools.")
	}
	if family == "rhel" {
		warnings = append(warnings, "AlmaLinux and Rocky Linux versions come from dnf module streams; the stream names in the catalog are unverified.")
	}
	if b.web == "caddy" {
		warnings = append(warnings, "Caddy comes from the distribution package; the version differs per release (older packages such as 2.6 do not know client_ip_headers, which the real-IP option uses). The build validates the generated Caddyfile and fails on an unsupported directive.")
	}
	if b.web == "apache" {
		warnings = append(warnings, "The Apache configuration is a minimal generated file with distribution-specific module paths (unverified); httpd -t runs during the build.")
	}
	if ng, ok := sel["nginx"]; ok {
		if p, ok2 := sel["php"]; ok2 {
			body, up := sizeBytes(fmt.Sprint(ng.Settings["clientMaxBodySize"])), sizeBytes(fmt.Sprint(p.Settings["upload_max_filesize"]))
			if body > 0 && up > body {
				warnings = append(warnings, "nginx clientMaxBodySize is smaller than PHP upload_max_filesize: large uploads are rejected by nginx first.")
			}
		}
	}
	for _, e := range []struct{ id, version, msg string }{
		{"php", "8.1", "PHP 8.1 reached upstream end of life on 2025-12-31 (endoflife.date); only a distribution may still patch it."},
		{"python", "3.10", "Python 3.10 reaches upstream end of life in October 2026 (endoflife.date); only a distribution may still patch it."},
		{"nginx", "1.18", "nginx 1.18 is far behind the current stable line; only the distribution patches it."},
	} {
		if s, ok := sel[e.id]; ok && s.Version == e.version {
			warnings = append(warnings, e.msg)
		}
	}
	port := fmt.Sprintf("unprivileged port %d", webPort)
	notes = []string{
		fmt.Sprintf("Runs as non-root user app (uid %d); servers listen on unprivileged ports (web %s), write only below /tmp and their data directories; HOME is /tmp.", appUID, port),
		"No secrets, credentials, build arguments or environment secrets enter the Dockerfile or the context; settings are typed and allow-listed, nothing is passed to a shell.",
		"Packages come only from the distribution's own repositories; no download scripts, no third-party repositories, no package recommends (apt) or documentation (dnf). The only other build-time download is the Minecraft server jar from the official host pinned in the catalog, verified by hash.",
		"Package caches are removed in the same layer; all configuration files are generated from the recipe and shown in the preview.",
		"Reproducibility: same recipe + catalog version + generator version = identical Dockerfile and context (recipe digest and context hash). Image bytes are identical only while the base digest and the distribution repositories are unchanged.",
	}
	return warnings, notes
}
