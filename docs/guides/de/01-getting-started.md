---
title: Erste Schritte und Installation
---

# Erste Schritte und Installation

[Übersicht](index.md) | Weiter: [Hosts und Agenten einbinden](02-hosts-and-agents.md)

## Was Sie heute bekommen

Der Manager ist ein einzelnes Go-Programm, das die Web-Oberfläche und die HTTP-API (`/api/v1`) ausliefert. Er läuft in drei Betriebsarten:

| Betriebsart | Was funktioniert | Stufe |
| --- | --- | --- |
| Prototyp (ohne `-config`) | Oberfläche, `GET /api/v1/health`, `GET /api/v1/version`. Kein Zustand, kein Docker. | Auf dem Entwicklungsrechner über Loopback ausgeliefert (W02); kein Container- oder VM-Lauf. |
| Auth-Modus (`auth.enabled`, ohne `manager`-Abschnitt) | Einrichtung, Passkey-Anmeldung, Wiederherstellung, Rollen, Audit ins Log. Zustand **nur im Speicher**, beim Neustart weg; Ressourcen-Endpunkte antworten 503. | [Fakes] (Software-Authenticator) |
| Verwalteter Modus (`manager`-Abschnitt) | Replizierter Speicher, Swarm-Anbindung, Agenten-Deployment, Cluster-Operationen, Backups, Monitoring. | [Lab-VMs] als Einzelinstanz, als Prozess auf einem Swarm-Manager-Knoten (E4-E6). |

![Übersichtsseite der Weboberfläche](../../assets/screenshots/de-light-overview.png)

*Bildschirmfoto aus dem Entwicklungs-Mock-Server (`npm run dev:mock`, erfundene Beispieldaten), nicht von einem echten Manager.*

## Voraussetzungen

- Ein Docker Swarm (E4-E6: Docker 29.9.2 auf Ubuntu 26.04, arm64). Die Kompatibilitätsmatrix kennt nur Docker 29.x. Für amd64 gibt es **keinen** Nachweis [Ungetestet].
- Ein TLS-Reverse-Proxy vor dem Manager. Passkeys (WebAuthn) brauchen einen HTTPS-Origin, dessen Host der konfigurierte `rpId` ist; reines `http://localhost` gilt nur für die Entwicklung. Der Manager hat für Oberfläche und API kein eigenes TLS.
- Ein Swarm-Manager-Knoten, an den der Manager gebunden wird. `deploy/stack.yaml` bindet dort `/var/run/docker.sock` ein; das entspricht Root-Rechten auf diesem Knoten. Der Manager läuft als uid 65532 mit der Gruppe des Sockets, schreibgeschützt und ohne Capabilities ([ADR 0014](../../decisions/0014-manager-container-docker-access.md), englisch). Den Socket in nichts anderes einbinden.

## Manager installieren

Technische Details und Paketierungsstand: [INSTALL.md](../../INSTALL.md) (englisch). Kurzfassung:

1. Image beschaffen. Es gibt noch kein Release; lokal aus dem Repository-Wurzelverzeichnis bauen (dedizierte Testumgebung und expliziter Docker-Kontext): `docker buildx build -f deploy/Dockerfile -t db-cluster-manager:local --build-arg VERSION=0.0.0-dev --load .` Die Release-Pipeline ist für [Prüfsummen, SBOMs, Provenienz und Image-Signaturen](../../RELEASING.md) eingerichtet, wurde aber noch nicht ausgeführt. Labor E7 baute das Manager-Image auf arm64; kein Release-Image wurde verifiziert.
2. Konfiguration erzeugen: `manager init -rp-id <Hostname der Oberfläche> -manager-addr <Adresse des Manager-Knotens> -out manager.json`. Der Befehl schreibt eine JSON-Konfiguration ohne geheime Werte (nur Namen von Swarm-Secrets) und gibt die weiteren Schritte aus: Host-Verzeichnisse, anzulegende Secrets und Netzwerke, den Deploy-Befehl und wo das Einrichtungstoken erscheint. `-backup -recovery-key-out <Datei>` richtet zusätzlich Backups mit einem neu erzeugten Wiederherstellungsschlüssel ein. Die Felder stehen im Kopfkommentar von `cmd/manager/main.go`; unbekannte Felder werden abgelehnt. Der Befehl ist nur durch Tests abgedeckt [Fakes].
3. Als Swarm-Config speichern (`docker config create dbcm-manager-config-1 manager.json`) und bereitstellen: `DBCM_MANAGER_NODE=<Hostname des Knotens> DBCM_DOCKER_GID=$(stat -c %g /var/run/docker.sock) DBCM_CONFIG_NAME=dbcm-manager-config-1 docker stack deploy -c deploy/stack.yaml dbcm` [Ungetestet]. Der Stack startet **ein** Replikat, fest an diesen Knoten gebunden (sein Datenvolume existiert nur dort), im Host-Netzwerk mit der API auf `127.0.0.1:8080` für Ihren TLS-Reverse-Proxy. `deploy/compose.yaml` ist die Variante für einen einzelnen Host. E4 betrieb dieselbe Konfiguration als transiente systemd-Unit auf einem Swarm-Manager-Knoten, nicht als Container.

### Hochverfügbarer Manager

Drei Replikate brauchen drei einzeln gebundene Dienste mit eigenen Konfigurationen, Schlüsselbund und Metastore-Peer-Zertifikaten; [INSTALL.md](../../INSTALL.md) („Three-replica variant“) beschreibt den Aufbau. Er wird nicht als Datei mitgeliefert und lief nie [Ungetestet]. Ein replizierter Metastore mit mehreren Replikaten wurde nur als Labor-Werkzeug `metad` (E3) und als Bibliothekscode nachgewiesen.

## Erster Start: Einrichtungstoken und erster Passkey

1. Manager mit aktivierter Authentifizierung starten. Beim Start erscheint eine Zeile auf stderr (Container: `docker logs`): `First-run setup token (valid until the first administrator is created): <Token>`. Im verwalteten Modus wird das Token nur ausgegeben, solange es keinen Administrator gibt.
2. Oberfläche über Ihre HTTPS-URL öffnen. Die Einrichtungsseite fragt nach Token, Konto-ID und Passkey-Bezeichnung und startet die Passkey-Registrierung des Browsers.
3. Danach existiert der erste Administrator, die Einrichtung ist dauerhaft gesperrt, eine Sitzung beginnt, und **zehn Wiederherstellungscodes werden genau einmal angezeigt**. Bewahren Sie sie offline auf (Passwortmanager oder Ausdruck). Es werden nur Hashes gespeichert.
4. Später melden Sie sich mit dem Passkey an. Weitere Passkeys lassen sich über das Konto registrieren (`/auth/passkeys/*`).

Prüfstand: Der API-Ablauf lief auf Lab-VMs mit einem ES256-Software-Authenticator (Konto angelegt, 10 Codes, Sitzung, `setupRequired: false`) [Lab-VMs]. Der UI-Ablauf lief einmal in headless Chromium mit virtuellem Authenticator [Fakes]. Ein echter Hardware-Authenticator, ein Plattform-Passkey, Safari/Firefox und Screenreader wurden **nicht** getestet [Ungetestet]. Falsche Einrichtungstokens werden je Client-IP begrenzt.

Token verloren: Manager neu starten (solange kein Administrator existiert, wird ein neues Token ausgegeben). Alle Passkeys verloren: siehe [Sicherheitsmodell](08-security-model.md).

## Oberfläche mit der Tastatur bedienen

Mit Tab erreichen Sie Sprachwahl, Navigation und Seitenaktionen. Der Sprunglink am Seitenanfang führt zum Hauptinhalt. Auf schmalen Bildschirmen öffnen Sie die Navigation über die Menüschaltfläche; Escape schließt sie. Nach einem Seitenwechsel wird die neue Überschrift fokussiert. Tabellen lassen sich fokussieren und horizontal mit der Tastatur verschieben. In den wichtigsten Formularen für Cluster, Dienste, Dashboards und Benachrichtigungen wird bei Eingabefehlern eine Zusammenfassung angesagt und das erste ungültige Feld fokussiert. Diese Funktionen wurden statisch und mit Unit-Tests geprüft; ein Browser- oder Screenreader-Test für diese Änderung steht noch aus.

## Nächste Schritte

Swarm anbinden und Hosts einbinden: [Hosts und Agenten einbinden](02-hosts-and-agents.md). Lesen Sie vor jedem Vertrauen [Bekannte Grenzen](10-known-limits.md).
