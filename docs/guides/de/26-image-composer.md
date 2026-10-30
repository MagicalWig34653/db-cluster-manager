---
title: Image-Composer
---

# Image-Composer [Nur mit Fakes und Mock-Server geprüft; daraus wurde noch kein Image gebaut]

Mit dem Image-Composer ([ADR 0026](../../decisions/0026-image-composer.md), Anforderung X17) stellen Sie den Inhalt eines Service-Images per Auswahl zusammen, statt ein Dockerfile zu schreiben: Basis-Distribution, nginx mit Version und Einstellungen, PHP mit Version, Erweiterungen, `php.ini`- und PHP-FPM-Pool-Einstellungen, Composer und einige Hilfsprogramme. Der Manager prüft die Auswahl, erzeugt Dockerfile und Konfigurationsdateien, zeigt sie zur Durchsicht und baut erst, nachdem ein Administrator genau diesen Plan freigegeben hat, über den vorhandenen Image-Build-Dienst (die Seite **Image-Builds** listet die entstandenen Jobs).

**Stand.** Nur Fakes und der Entwicklungs-Mock-Server. Der Komponentenkatalog (Version 2026.10.29-2) ist als `unverified` markiert: Paketnamen und Versionen wurden aus Wissen geschrieben und nicht gegen echte Paketquellen geprüft, und aus einem erzeugten Rezept wurde noch kein Image gebaut. Der echte Build ist die letzte Prüfung und kann einen Eintrag ablehnen. Der Managed-Manager kann die Endpunkte hinter `manager.imageComposer.enabled` bereitstellen (Standard aus; braucht `auth.enabled` und `manager.imageBuild`); bei ausgeschaltetem Schalter oder ohne Docker-Builder antworten sie mit 503. Die Verdrahtung ist nur mit Fakes geprüft (kein echter Build).

## Die Seite benutzen

Öffnen Sie **Image-Composer** in der Navigation.

1. **Basis-Image.** Vergeben Sie einen Namen, wählen Sie Distribution und Release (Alpine 3.20 bis 3.22, Debian 12 und 13, Ubuntu 22.04 und 24.04 LTS, AlmaLinux 9, Rocky Linux 9) und tragen Sie den Digest des Basis-Images ein (`sha256:` plus 64 Hex-Zeichen). Ein schwebender Tag wird nie gebaut. Den Digest ermitteln Sie selbst, zum Beispiel mit `docker buildx imagetools inspect docker.io/library/alpine:3.20`.
2. **Komponenten.** Haken Sie die gewünschten Komponenten an. Die Versionsliste zeigt nur Versionen, die es auf dem gewählten Release gibt. Erweiterungen hängen von der Distributionsfamilie ab. Eine Komponente, die eine andere braucht (Composer braucht PHP), lässt sich nicht ohne sie wählen. Genau eine Komponente ist der Hauptprozess (nginx, Apache, Caddy, Node.js, Python mit gunicorn, Java, SFTP oder der Job-Runner); wählen Sie eine zweite, wird die erste abgewählt. PHP-FPM ist ein Begleitprozess neben einem Webserver oder der einzige Hauptprozess (FPM oder ein CLI-Worker-Skript), wenn kein anderer Hauptprozess gewählt ist; PHP lässt sich nicht mit Node.js, Python, Java, SFTP oder dem Job-Runner kombinieren.
3. **Einstellungen.** Jede Komponente zeigt ihre erlaubten Einstellungen mit Standardwert, zulässigem Bereich und gegebenenfalls einem Hinweis. Gesendet werden nur Einstellungen, die vom Standard abweichen. Einstellungen, die nicht greifen können, sind ausgeblendet (zum Beispiel `opcache.*` ohne die Opcache-Erweiterung oder das FastCGI-Timeout ohne PHP).
4. **Durchsicht.** Rechts folgt die Antwort des Managers jeder Änderung (etwa 300 ms nach der letzten Eingabe): Befunde mit Pfad, die Varianten je Distribution, das erzeugte Dockerfile und jede Konfigurationsdatei (Datei in der Liste wählen), die Paketliste, Grenzen vor dem Bauen sowie Hinweise zu Härtung und Reproduzierbarkeit. Der Rezept-Digest identifiziert genau dieses Rezept.
5. **Varianten.** Jedes Release steht als *Kompatibel* oder *Weicht ab* in der Liste, mit dem, was abweicht, und der neuesten dort verfügbaren Version. *Diese Basis verwenden* wechselt die Basis und setzt Versionen auf das, was das Release bietet; Erweiterungen oder Komponenten, die es dort nicht gibt, entfallen, und das sehen Sie sofort.
6. **Planen und bauen.** Wählen Sie Registry, Repository und Tag und klicken Sie auf *Build planen*. Der Plan zeigt Schritte, Warnungen und den Plan-Digest. Setzen Sie die Freigabe und klicken Sie auf *Image bauen*. Jede Änderung an Rezept, Ziel oder Plattformen macht den Plan ungültig. Planen und Bauen brauchen die Administratorrolle; Operatoren können Rezepte ansehen.

## Was der Generator garantiert

- **Gleiches Rezept, gleiche Ausgabe.** Dockerfile und Kontextdateien sind für dasselbe normalisierte Rezept, dieselbe Katalog- und Generatorversion byteidentisch (Golden-File-Tests). Das Image selbst ist nur identisch, solange Basis-Digest und Paketquellen der Distribution unverändert sind: Pakete sind auf die Version major.minor eingeschränkt, ihre Patchstände schwimmen.
- **Härtungsstandards.** Basis per Digest festgelegt, Nicht-Root-Benutzer (uid 10001), unprivilegierter Port 8080 (nginx) oder 9000 (nur FPM), keine Geheimnisse oder Build-Argumente, keine Download-Skripte, keine Fremd-Repositories, `server_tokens` aus, Sicherheits-Header an, `expose_php`, `display_errors` und `allow_url_fopen` standardmäßig aus, `allow_url_include` aus und `cgi.fix_pathinfo=0` immer. `nginx -t` und `php-fpm -t` laufen beim Build.
- **Keine beliebige Shell.** Einstellungen sind typisiert (Ja/Nein, ganze Zahl mit Bereich, Auswahl aus einer Liste oder Text mit verpflichtendem Muster). Unbekannte Schlüssel, falsche Typen, Zahlen außerhalb des Bereichs und Shell-Sonderzeichen in einem musterprüften Feld werden mit dem Feldpfad abgelehnt.
- **Ein Hauptprozess.** Im Image gibt es keinen Supervisor. Stirbt PHP-FPM, während nginx weiterläuft, antworten Anfragen mit 502; nutzen Sie den Health-Check des Dienstes. Ein reines PHP-FPM-Image lauscht ohne Authentifizierung auf 9000: Betreiben Sie es in einem isolierten Netz.

## API

| Route | Rolle | Zweck |
| --- | --- | --- |
| `GET /api/v1/image-composer/catalog` | Viewer | Distributionen, Komponenten, Matrix, Optionen, Einstellungsschema (Beschriftungen DE/EN) |
| `POST /api/v1/image-composer/preview` | Operator | Prüfen und erzeugen; ungültige Rezepte liefern `valid: false` und `issues` |
| `POST /api/v1/image-composer/builds` | Administrator | Ohne `approvalDigest`: Build-Plan (200); mit dem Digest des Plans: startet den Build (202). Geändertes Rezept oder Ziel ergibt 409 `approval_mismatch` |

Ein Rezept hat `name`, `base {distro, release, digest}`, optional `platforms` und `components [{id, version, features, settings}]`. Das vollständige Schema steht in `api/openapi.yaml`.

## Aktivieren

```json
"imageComposer": { "enabled": false }
```

`enabled` auf true setzen. Es braucht `auth.enabled` und `manager.imageBuild` (Builds laufen über den Image-Build-Dienst und seinen Freigabeablauf); sonst startet der Manager nicht. Ohne Docker-Builder bleiben die Endpunkte bei 503. Das Rezept wird immer auf dem Server erzeugt; der Client sendet nie ein Dockerfile.

## Grenzen

Nur Fakes; der Katalog ist ungeprüft; kein echter Build, kein SBOM, kein Scan (das SBOM bleibt `not_generated`, die Paketliste ist seine vorgesehene Eingabe). Sieben Basen, 39 PHP-Erweiterungen aus Distributionspaketen, kein PECL und keine Quell-Builds, keine eigenen Webserver-Snippets, ein PHP-FPM-Pool, eine Site, kein Anwendungscode im Image (siehe Abschnitt zur Katalogerweiterung, was bewusst fehlt). Die Seite ist nur gegen den Mock-Server geprüft (Screenshots müssen die Mock-Kennzeichnung tragen).

## Katalogerweiterung (Version 2026.10.29-2)

Alles Folgende wird aus Distributionspaketen und typisierten Einstellungen erzeugt; nichts davon wurde gebaut oder ausgeführt (siehe Stand oben).

**Basen.** Alpine 3.20, 3.21 und 3.22, Debian 12 und 13 (trixie), Ubuntu 22.04 und 24.04, AlmaLinux 9 und Rocky Linux 9 (dieselben dnf-Modulströme wie AlmaLinux). Nicht jede Komponente gibt es auf jeder Basis: Die Versionsliste zeigt nur, was der Katalog dort erwartet, und die *Varianten* zeigen den Rest. Komponenten, deren Distributionsversion schon abgelaufen ist, fehlen (Node.js auf Debian 12 und Ubuntu); PHP 8.1, Python 3.10 und nginx 1.18 werden mit einer Warnung angeboten.

**Webserver (Hauptprozess).** nginx, Apache httpd (Event-MPM, generierte Minimalkonfiguration) und Caddy 2 (Admin-Endpunkt und automatisches HTTPS aus, reines HTTP hinter Ihrem Proxy). Alle drei betreiben PHP-FPM als Begleitprozess. Gemeinsame Einstellungen: Servername, Dokumentenwurzel, maximale Body-Größe, Kompression, Preset der Sicherheits-Header (`basic`, `strict` mit HSTS, Permissions-Policy und COOP, `api` mit no-store und geschlossener CSP), echte Client-Adresse aus `X-Forwarded-For` oder `X-Real-IP`, begrenzt auf die aufgeführten vertrauenswürdigen Proxy-Bereiche, Zugriffslog an oder aus. nginx ergänzt optional Brotli (Distributionsmodul), ein Rate-Limit je Client-Adresse mit Burst (Antwort 429) und `trustForwardedProto` (setzt das PHP-Flag `HTTPS`, wenn TLS an Ihrem Proxy endet; nur aktivieren, wenn jede Anfrage über den Proxy kommt). TLS wird im Image nie terminiert.

**PHP.** PHP 8.1 bis 8.4 je nach Basis; 39 Erweiterungen, darunter `apcu`, `imagick`, `redis` und `memcached`, soweit die Distribution sie paketiert (auf AlmaLinux/Rocky nicht `imagick`, `redis`, `memcached`); Opcache-Einstellungen einschließlich `validate_timestamps`, `revalidate_freq`, `max_accelerated_files`, `interned_strings_buffer` und JIT (`opcache.jit`, Puffergröße), `apc.shm_size`, `max_file_uploads`, `realpath_cache_size`, `pm.process_idle_timeout`. Die Betriebsart **worker** macht ein PHP-CLI-Skript (zum Beispiel einen Queue-Consumer) zum einzigen Hauptprozess; sie lässt sich nicht mit einem Webserver kombinieren. Mehrere PHP-FPM-Pools gibt es nicht: Das Image hat eine Site (siehe Nachtrag im ADR).

**Anwendungslaufzeiten (Hauptprozess).** Node.js 20 oder 22 (Einstiegsskript, Port, `NODE_ENV`, Heap-Limit, optional npm), Python 3 mit gunicorn (WSGI `Modul:Aufrufbar`, Worker, Threads, Timeout, vertrauenswürdige Proxys, optional pip; nicht auf AlmaLinux/Rocky) und Java 17 oder 21 JRE (Jar-Pfad, Heap-Prozent, Garbage Collector, Beenden bei Speichermangel). Die Anwendung ist **nicht** im Image: Binden Sie sie unter `/app` ein oder bauen Sie ein abgeleitetes Image. Diese Laufzeiten lassen sich nicht mit PHP kombinieren.

**Speicher- und Hilfs-Images.** *SFTP* (OpenSSH, nur das interne SFTP-Subsystem, Port 2222, ein Konto, kein Chroot): Host-Key und authorized_keys kommen als eingebundene Secrets (`/run/secrets/...`), nicht aus dem Image. *Zeitgesteuerter Job-Runner*: eine generierte Shell-Schleife, die den voreingestellten Job `restic-backup` oder `rclone-sync` in festem Abstand ausführt; Repository, Passwortdatei und rclone-Konfiguration kommen aus Umgebung oder Secrets des Services. *Backup- und Sync-Werkzeuge* (restic, rclone, rsync, OpenSSH-Client, MinIO-Client nur auf Alpine) und *Datenbank-Client-Werkzeuge* (MariaDB, PostgreSQL, redis-cli, SQLite) sind Paketauswahlen ohne Prozess.

**Systemeinstellungen.** Zeitzone (`TZ`), Locale (`C.UTF-8`, `en_US.UTF-8`, `de_DE.UTF-8`; erzeugt auf Debian, Ubuntu und AlmaLinux/Rocky, auf Alpine nur `LANG`), zusätzliche CA-Zertifikate als PEM (jedes muss ein CA-Zertifikat sein; private Schlüssel und Endzertifikate werden abgelehnt; sie werden in den System-Truststore installiert) und ein Container-Health-Check: `http` (curl gegen den Haupt-Webport oder den Laufzeit-Port) oder `fpm-ping` (nginx mit PHP-FPM fragt PHP-FPM selbst über einen nur lokalen Ping-Endpunkt, damit ein toter PHP-FPM auffällt).

**Bewusst nicht angeboten.** Statische Go-Binaries auf `scratch` oder Distroless: Diese Basen haben keinen Paketmanager, und der Composer kann noch kein Binary sicher in den Build-Kontext bringen (ein per Digest festgelegtes Payload-Image wäre der nächste Schritt, eine Produktentscheidung). vsftpd und Chroot-SFTP mit mehreren Konten: Beides braucht Root oder PAM, um Benutzer zu authentifizieren und zu wechseln, was die Nicht-Root-Regel verbietet; FTP ist zudem unverschlüsselt und hinter dem Swarm-Routing unhandlich. Mehrere PHP-FPM-Pools: eine Site je Image.

**Ungeprüfte Annahmen.** Neben den Paketnamen setzt der Katalog distributionsspezifische Dateipfade (Apache-Modulverzeichnisse, nginx-Brotli-Modulpfad) und Paketinhalte voraus (zum Beispiel dass sshd ohne Root im Container läuft und das Caddy-Paket die erzeugten Direktiven kennt; Debian 12 und Ubuntu 24.04 liefern vermutlich ein älteres Caddy, das `client_ip_headers` nicht kennt). Jedes erzeugte Image führt einen Konfigurationstest aus, wo das Werkzeug einen bietet (`nginx -t`, `httpd -t`, `caddy validate`, `php-fpm -t`, `sshd -t`); eine falsche Annahme scheitert also beim Build, nicht beim Start. Das Bestehen von Debian 13, Rocky/AlmaLinux 9 und PHP 8.4 wurde am 29.10.2026 gegen endoflife.date geprüft; Alpine-Daten ließen sich nicht abrufen. Kein Paketname wurde geprüft.
