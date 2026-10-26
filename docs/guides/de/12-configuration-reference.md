---
title: Konfigurationsreferenz
---

# Konfigurationsreferenz

[Übersicht](index.md) | Zurück: [Betriebshandbuch](11-operator-runbook.md)

Abgeleitet aus `cmd/manager/main.go`, `cmd/manager/managed.go` und `cmd/manager/peers.go` (Stand 2026-10-25). Der Manager liest eine JSON-Datei, angegeben mit `-config <Datei>` oder der Umgebungsvariable `DCM_CONFIG`. **Unbekannte Felder werden abgelehnt**, der Manager startet dann nicht. Echtes JSON kennt keine Kommentare; die `//`-Hinweise unten dienen nur der Erklärung.

Prüfstufe: Parsen und Validierung sind unit-getestet [Fakes]. Die Abschnitte `auth`, `manager`, `manager.agentTransport`, `manager.backup` und `manager.settings` liefen in der Konfiguration von E4-E6 auf Lab-VMs (eine Instanz als Prozess auf einem Swarm-Manager-Knoten) [Lab-VMs]. Drei Manager-Prozesse mit `keyring` und `peerTLS` liefen auf Labor-VMs (E4 B2) [Lab-VMs]; die neuen Erneuerungs-/Weiterleitungsfelder und der Drei-Dienste-Stack haben nur Fake-/Statik-Nachweise [Fakes].

## Kommandozeile

| Option | Standard | Bedeutung |
| --- | --- | --- |
| `-listen addr` | `127.0.0.1:8080` | Adresse von UI und API; eigenes HTTPS mit `manager.tls`, sonst Loopback-HTTP hinter einem TLS-Proxy. |
| `-config file` | `$DCM_CONFIG` | JSON-Konfiguration. Ohne sie läuft der Manager im Prototyp-Modus. |
| `-version` | | Version ausgeben und beenden. |
| `-healthcheck` | | Loopback-Health-Endpunkt am Port von `-listen` abfragen; HTTPS mit TLS-Konfiguration, sonst HTTP; bei Gesundheit Exit 0. |
| `-print-replica-key` | | Öffentliche Datenschlüssel-Identität dieses Replikats ausgeben (Datei `replica.key` in `manager.dataDir`, wird bei Bedarf angelegt) und beenden. Braucht `-config` mit `manager.dataDir`. |
| `-issue-peer-certs dir` | | Metastore-Peer-CA und je ein Zertifikat pro Replikat in `dir` anlegen, dann beenden. Die CA-Passphrase kommt aus der Umgebungsvariable `DBCM_PEER_CA_PASSPHRASE` (mindestens 12 Zeichen). Überschreibt keine vorhandene CA. |
| `-installation id` | | Installations-ID für `-issue-peer-certs` (dort Pflicht). |
| `-peer-ids a,b,c` | | Kommagetrennte Replikat-IDs für `-issue-peer-certs` (dort Pflicht; Buchstaben, Ziffern, `.`, `_`, `-`, höchstens 63 Zeichen, Beginn mit Buchstabe oder Ziffer). |
| `-peer-validity d` | `8760h` (365 Tage) | Gültigkeit der Zertifikate für `-issue-peer-certs`. |

## Modi

| Konfiguration | Modus | Zustand |
| --- | --- | --- |
| Keine Datei, oder `auth.enabled` false und kein `manager` | Prototyp | Nur `GET /api/v1/health`, `GET /api/v1/version` und die UI. |
| `auth.enabled` true, kein `manager` | Auth-Modus | Konten, Sitzungen und Audit **im Speicher**; Ressourcen-Endpunkte antworten 503. |
| Abschnitt `manager` | Verwalteter Modus | Replizierter Metastore auf Platte; der Leader bedient die Befehls-API. Ohne `auth.enabled` gibt es keine Anmeldung und damit keine nutzbare API. |

## `auth`

| Feld | Typ | Standard | Pflicht | Bedeutung |
| --- | --- | --- | --- | --- |
| `enabled` | bool | `false` | ja im verwalteten Modus | Schaltet Einrichtung, Passkey-Anmeldung, Wiederherstellung und die volle API ein. |
| `rpId` | string | | ja, wenn aktiviert | WebAuthn-Relying-Party-ID, normalerweise der DNS-Name Ihrer HTTPS-URL. |
| `rpDisplayName` | string | | empfohlen | Name, den die Passkey-Abfrage zeigt. |
| `origins` | Liste | | ja, wenn aktiviert | Erlaubte Origins: `https://`-URLs, deren Host `rpId` oder eine Subdomain ist; `http://localhost[:port]` nur für die Entwicklung. |

```json
{ "auth": { "enabled": true, "rpId": "db.example.com", "rpDisplayName": "cluster-manager", "origins": ["https://db.example.com"] } }
```

## `manager`

Der Abschnitt wählt den verwalteten Modus. Pflicht: `dataDir` und `bind`.

| Feld | Typ | Standard | Bedeutung |
| --- | --- | --- | --- |
| `nodeId` | string | `manager-1` | ID dieses Replikats. Muss zum Eintrag in `peers` und, bei `peerTLS`, zur Zertifikatsidentität passen. |
| `dataDir` | string | keiner, **Pflicht** | Metastore (`<dataDir>/metastore`), `data.key`, `replica.key` und der Standard-Monitoringspeicher. Sichern, siehe [Betriebshandbuch](11-operator-runbook.md). |
| `bind` | string | keiner, **Pflicht** | TCP-Adresse des Metastores, zum Beispiel `127.0.0.1:7946`. Ohne `peerTLS` ist der Transport unverschlüsseltes TCP: nur Loopback oder isolierte Netze. |
| `advertise` | string | leer | Adresse, die Peers anwählen, falls sie von `bind` abweicht. |
| `peers` | Liste von `{id, address, apiAddress?}` | leer | Anfängliche stimmberechtigte Mitgliedschaft **einschließlich dieses Replikats**; leer bedeutet Einzelinstanz. Im Standardbetrieb sind Mitgliedschaftsänderungen nicht verfügbar. |
| `bootstrap` | bool | `false` | Schreibt beim ersten Start einer neuen Installation die Anfangsmitgliedschaft (`peers`); wird ignoriert, wenn das Replikat schon Zustand auf Platte hat. Alle bootstrappenden Replikate müssen dieselbe Liste übergeben, also bei einer neuen Installation auf jedem Replikat `true`. |
| `keyFile` | string | `<dataDir>/data.key` | 32-Byte-Datenschlüssel für Geheimnisse im Ruhezustand. Ohne `keyring` wird er bei Bedarf angelegt und muss von Hand auf jedes Replikat kopiert werden. Mit `keyring` wird eine vorhandene Datei beim Bootstrap des Schlüsselrings als erster Schlüssel übernommen; eine neue wird nicht angelegt. |
| `keyring` | bool | `false` | Replizierter, verpackter Datenschlüsselring ([ADR 0013](../../decisions/0013-replica-keys-peer-tls-and-revocation.md)). Jedes Replikat hält nur seine Identität in `<dataDir>/replica.key`. Empfohlen bei mehr als einem Replikat. |
| `peerTLS` | Objekt | fehlt | Gegenseitiges TLS 1.3 zwischen Metastore-Replikaten, siehe unten. Empfohlen bei mehr als einem Replikat. |
| `apiForwardBind` | Host:Port | fehlt | mTLS-Listener für die API-Weiterleitung von Followern. Benötigt `peerTLS`, mindestens drei Peers und je eine `apiAddress`. Nicht für Clients freigeben. |
| `labAgents` | bool | `false` | Unauthentifizierte HTTP-Agentenendpunkte des Labors. **Nur Labor**; schließt `agentTransport` aus. Ohne beides werden Agentenbefehle abgelehnt. |
| `autoResume` | bool | `false` | Ein neuer Leader setzt übernommene Operationen fort. |
| `agentTransport` | Objekt | fehlt | Authentifizierter mTLS-Agententransport mit Einbindung ([ADR 0010](../../decisions/0010-agent-transport-and-enrollment.md)). Praktisch nötig für Hosts, Cluster und Backups. |
| `monitoring` | Objekt | aktiv | Metrikspeicher und Alarme. |
| `backup` | Objekt | fehlt | Manager-Seite der Cluster-Backups. |
| `imageBuild` | Objekt | fehlt | Aktiviert den Image-Build-Dienst (Entwurf, nur Fakes), siehe unten. Die Registry-Endpunkte funktionieren auch ohne. |
| `settings` | Objekt | fehlt | Installationseinstellungen, die der Leader bei jedem Leadership-Start in die replizierten Einstellungen einmischt (noch keine API dafür). |

### `manager.peers[]`

| Feld | Bedeutung |
| --- | --- |
| `id` | Replikat-ID, gleich dem `nodeId` dieses Replikats. |
| `address` | Metastore-Adresse dieses Replikats, wie die anderen sie anwählen. Mit `peerTLS` muss das Replikat unter dieser Adresse genau diese ID vorweisen. |
| `apiAddress` | Separate Adresse für die Manager-API-Weiterleitung. Replikate erreichen den aktuellen Leader darüber mit Peer-mTLS. `manager init -peers` nutzt Port 7447. |

### `manager.peerTLS`

Existiert der Abschnitt, sind alle vier Felder Pflicht. Erzeugt von `manager -issue-peer-certs`. Die Zertifikatsdatei wird neu gelesen, wenn sie sich ändert; ein neu ausgestelltes Zertifikat braucht keinen Neustart. Die Peers müssen in `peers` stehen; die Zertifikatsidentität ist `dbcm://<Installation>/metastore/<Replikat-ID>`.

| Feld | Bedeutung |
| --- | --- |
| `installation` | Installations-ID, auf jedem Replikat gleich und gleich `settings.installationId`. |
| `caFile` | Zertifikat der Peer-CA (`ca.pem`). |
| `certFile` | Kette dieses Replikats (`<id>/peer.pem`). |
| `keyFile` | Privater Schlüssel dieses Replikats (`<id>/peer-key.pem`). |
| `renew` | Optional `{caFile, passphraseFile, beforeDays?, validityDays?, intervalHours?}`. Versiegelte CA und Passphrase liegen als getrennte Online-Secrets vor. Standard: 30 Tage Vorlauf, 365 Tage neue Gültigkeit, Prüfung alle 12 Stunden. Dieselbe schreibbare PEM-Datei für `certFile` und `keyFile` ermöglicht atomaren Austausch. Fehler werden protokolliert und erneut versucht; ein abgelaufenes Zertifikat bleibt ungültig. |

### `manager.agentTransport`

`enrollListen` und `managerUrl` sind Pflicht.

| Feld | Typ | Standard | Bedeutung |
| --- | --- | --- | --- |
| `enrollListen` | string | keiner, **Pflicht** | TLS-Listen-Adresse des Einbindungs-Endpunkts, zum Beispiel `0.0.0.0:7444`. |
| `managerUrl` | string | keiner, **Pflicht** | `https://Host:Port`, unter der Agenten `enrollListen` erreichen. Ein IP-Host wird ins Zertifikat aufgenommen. |
| `serverDns` | Liste | leer | Zusätzliche DNS-Namen im Einbindungszertifikat. |
| `serverIps` | Liste | leer | Zusätzliche IP-Adressen im Einbindungszertifikat (müssen IPs sein). |
| `agentPort` | int | `7443` | Port, auf dem Agenten auf ihren Hosts lauschen. |
| `allowAnySource` | bool | `false` | Schaltet die Prüfung ab, dass eine Einbindungsanfrage von der Swarm-Adresse des genannten Hosts kommt. Nur hinter NAT nötig; schwächt die Einbindung. |
| `monitorUser` | string | leer | Datenbank-Monitoringkonto, das Agenten für die Zustandserfassung nutzen. Nötig für Zustandserfassung und die Totalausfall-Benachrichtigung. |
| `monitorSecret` | string | leer | Name eines vorhandenen Swarm-Secrets mit dem Passwort dieses Kontos. |
| `dbSecrets` | Liste | leer | Weitere vorhandene Swarm-Secrets, die in den Agentendienst eingehängt werden (Root- und Backup-Konto-Passwörter). Gilt nur beim Anlegen des Agentendienstes; die Secrets brauchen das Label `dbcm.installation=<Installations-ID>`. |

### `manager.monitoring`

| Feld | Typ | Standard | Bedeutung |
| --- | --- | --- | --- |
| `disabled` | bool | `false` | Schaltet Metrikerfassung, Alarmierung und Metrikabfragen ab; die Benachrichtigungseinstellungen funktionieren weiter. |
| `dataDir` | string | `<manager.dataDir>/monitoring` | Metrikspeicher; muss ein absoluter Pfad sein. |

### `manager.backup`

| Feld | Typ | Standard | Bedeutung |
| --- | --- | --- | --- |
| `backendConfigFile` | string | leer | Absoluter Pfad einer JSON-Datei mit demselben Inhalt wie das Backend-Secret der Jobs: `{"type": "sftp" oder "s3" oder "dir", ...}` plus ein Abschnitt `sftp`, `s3` oder `dir` (Hostschlüsselprüfung ist bei SFTP Pflicht). Ohne sie bleiben Backup-Bedingungen unerfüllt und Wiederherstellungen werden abgelehnt (`settings_incomplete`). `dir` taugt nur für Labortests. |
| `scheduleIntervalSeconds` | int | `60` | Wie oft der Leader Zeitpläne, Aufbewahrung und Ablauf von Inspektionen prüft; mindestens 10. |

### `manager.imageBuild`

Ist der Abschnitt vorhanden, sind Image-Builds aktiv (`POST /image-builds`); ohne ihn antworten die Build-Routen mit 503, `/registries` funktioniert weiter. Der Manager führt `docker buildx build` als eigenen Prozess aus. Das mitgelieferte Manager-Image enthält kein Docker-CLI und der Stack gibt ihm kein BuildKit; für einen Build muss beides zuerst ergänzt werden [Ungetestet]. `manager init -image-build` schreibt den Abschnitt mit den Standardwerten.

| Feld | Typ | Standard | Bedeutung |
| --- | --- | --- | --- |
| `dockerBinary` | string | `docker` | Pfad oder Befehlsname des Docker-CLI (ohne Argumente). |
| `workDir` | string | Temp-Verzeichnis des Systems | Absolutes Verzeichnis für temporäre Build-Kontexte und Docker-Configs (diese enthalten die Registry-Zugangsdaten, Modus 0600, nach dem Build gelöscht). |
| `maxLogBytes` | int | `262144` | Obergrenze des gespeicherten, geschwärzten Job-Protokolls; mindestens 1024. |

Registry-Zugangsdaten, Job-Aufzeichnungen und Pläne liegen im replizierten Speicher; Zugangsdaten werden wie Sitzungs-Digests mit dem Datenschlüssel (`keyFile` oder `keyring`) versiegelt.

### `manager.settings`

Der Leader mischt sie bei jedem Leadership-Start ein; leere Felder lassen gespeicherte Werte unberührt. `installationId` ist nach dem Setzen unveränderlich; `volumeBase` ändert sich nicht, solange ein Cluster existiert.

| Feld | Typ | Bedeutung |
| --- | --- | --- |
| `installationId` | string | Kennzeichnet Objekte dieser Installation (Label `dbcm.installation`). **Pflicht, bevor ein Cluster angelegt werden kann.** |
| `volumeBase` | string | Sauberes absolutes Verzeichnis auf jedem Host, unter dem die Knoten-Volumes liegen. **Pflicht, bevor ein Cluster angelegt werden kann.** Muss auf jedem Host existieren. |
| `agentImage` | string | Image des Agentendienstes. **Pflicht zum Bereitstellen der Agenten.** |
| `monitorSocket` | string | Relativer Pfad (unterhalb der Volume-Wurzel) des Monitoring-Sockets des Knotens. |
| `accounts` | Objekt | Datenbankkonten neuer Cluster: `monitorUser`, `monitorSecret`, `backupUser`, `backupSecret` (Namen von Swarm-Secrets) und `backupLogical` (bool, vergibt zusätzlich die Leserechte für `mariadb-dump`). |
| `backup` | Objekt | Backup-Einstellungen, siehe unten. |

`settings.backup` (alle Pflicht, bevor ein Backup starten kann): `jobImage` (Image mit `dbcm-job` und den Backup-Werkzeugen), `backendSecret` (Swarm-Secret mit dem Backend-JSON), `dbCredentialSecret` (Zugangsdaten des Backup-Kontos), `recipientKey` (öffentliche Hälfte des Recovery-Keys; Backups werden dafür verschlüsselt, derselbe Schlüssel ist der Wiederherstellungs-Umschlag des Datenschlüsselrings). Weiter: `keySecret` (Standard-Swarm-Secret mit dem Recovery-Key für Wiederherstellungen), `inspectionNetwork` (Nicht-Produktivnetz für Inspektionen; dafür nötig).

## Reaktion auf Schwachstellen

**Nur [Fakes]:** im Manager verdrahtet und mit Fakes und `httptest` getestet; kein echter Feed und kein echter KI-Anbieter wurde angesprochen. Der Abschnitt `manager.vuln` schaltet sie ein (braucht `auth.enabled`; fehlt er, antworten die Routen mit 503 und nichts läuft). Richtlinie, Anbieter und Zustimmung liegen im Metastore und werden in der Oberfläche oder über die API geändert (Berechtigung auf die Ressource `vuln`, nur Administrator-Voreinstellung).

`manager.vuln`:

| Feld | Typ | Standard | Bedeutung |
| --- | --- | --- | --- |
| `pollIntervalSeconds` | int | `3600` | Abfrageintervall der Feeds auf dem Leader, mindestens 300. Die erste Abfrage läuft 30 s nach Beginn der Leaderschaft. |
| `feeds` | Liste | leer | Advisory-Quellen, jede Art einmal: `{"kind": "osv" | "github" | "nvd" | "kev", "url": "https://..."}`, optional `apiKeyFile` (absoluter Pfad einer Datei mit Token oder API-Schlüssel; nur `github` und `nvd`), oder `{"kind": "osvFile", "path": "/abs/mirror.json"}` für einen Offline-OSV-Spiegel. Es gibt keine Standard-URLs; `http` wird nur für Loopback-Hosts akzeptiert. Kein Feed eingetragen: Es wird nichts abgerufen. |
| `defaultMode` | string | `notify_only` | Setzt die Richtlinie einmalig, wenn keine gespeichert ist: `notify_only` oder `approval_required`. Überschreibt nie eine gespeicherte Richtlinie; `auto_for_policy` lässt sich nur in der Oberfläche oder über die API setzen. |
| `providers` | Liste | leer | KI-Anbieter, die angelegt werden, wenn ihre ID noch nicht existiert: `id`, `name`, `kind` (`claude_api`, `openai_api` oder `openai_compatible`; CLI-Arten nur über die API), `baseUrl`, `models`, `defaultModel`, `apiKeyFile` (wird einmal gelesen und verschlüsselt im Metastore abgelegt), `limits`, `maxOutputTokens`, `timeoutSeconds`. Sie bleiben unbenutzbar, bis in der Oberfläche zugestimmt wird. |

Anfragen an Feeds und Anbieter folgen keinen Weiterleitungen. Benachrichtigungen (neuer Fund, ablaufende Abschwächung) laufen als Ereignisse der Art `vuln.*` über den Benachrichtigungs-Dispatcher und folgen deinen Routing-Regeln.

### API-Einstellungen

Richtlinie (`PUT /api/v1/vuln/policy`, bedingt auf `version`):

| Feld | Typ | Standard | Bedeutung |
| --- | --- | --- | --- |
| `mode` | string | `notify_only` | `notify_only`, `approval_required` oder `auto_for_policy`. |
| `provider`, `model` | string | leer | KI-Anbieter-ID und Modell für Analysen; beide oder keines. Leer heißt nur deterministische Vorschläge. |
| `auto.severities` | Liste | leer | Schweregrade, die automatisch freigegeben werden dürfen (nur `auto_for_policy`, Pflicht). |
| `auto.services` | Liste | leer | Namentlich genannte Dienste; kein Platzhalter (Pflicht). |
| `auto.kinds` | Liste | leer | `patched_rebuild` und/oder `mitigation` (Pflicht). |
| `auto.maxPerDay` | int | keiner | Automatische Freigaben je UTC-Tag, 1-20 (Pflicht). |
| `killSwitch` | bool | `false` | Stoppt alle automatischen Freigaben. |
| `mitigationDays` | int | `30` | Laufzeit einer Abschwächung, 1-90 Tage. |
| `retainPrompts` | bool | `false` | Reserviert; Prompts werden noch nicht aufbewahrt (nur ihre Hashes). |

KI-Anbieter (`POST /api/v1/vuln/providers`):

| Feld | Typ | Standard | Bedeutung |
| --- | --- | --- | --- |
| `id`, `name` | string | keiner | Kennung und Anzeigename. |
| `kind` | string | keiner | `claude_api`, `openai_api`, `openai_compatible`, `claude_code_cli`, `codex_cli`. |
| `baseUrl` | string | Claude: `https://api.anthropic.com`; OpenAI: `https://api.openai.com/v1` | Pflicht für `openai_compatible` (zum Beispiel `http://127.0.0.1:1234/v1`). Unverschlüsseltes http nur zu Loopback- oder privaten Adressen. |
| `models`, `defaultModel` | Liste, string | leer | Wählbare Modelle; leer erlaubt jeden wohlgeformten Namen. |
| `apiKey` | string | leer | Nur schreibbar; Pflicht für `claude_api` und `openai_api`, optional für `openai_compatible`, für CLI-Arten abgewiesen. Wird versiegelt gespeichert. |
| `binary`, `home` | string | leer | Nur CLI-Arten: absoluter Pfad der CLI und des Verzeichnisses mit ihrem Login-Zustand. |
| `experimental` | bool | `false` | Muss für CLI-Arten `true` sein. |
| `limits.requestsPerDay`, `limits.tokensPerDay` | int | `50`, `200000` | Verbrauchsgrenzen je UTC-Tag. |
| `maxOutputTokens` | int | `2048` | Obergrenze der Ausgabe-Token, höchstens 16384. |
| `timeoutSeconds` | int | `120` | Zeitlimit für Anfrage oder CLI, höchstens 600. |

Zustimmung: `PUT /api/v1/vuln/providers/{id}/consent` mit `{"grant": true, "statement": <egressStatement>}`; Widerruf mit `{"grant": false}`.

Feeds werden in `manager.vuln.feeds` konfiguriert (oben); keiner hat eine Standard-URL.

## Was der verwaltete Modus braucht

| Ziel | Nötige Konfiguration |
| --- | --- |
| Verwalteten Modus starten | `auth.enabled` mit `rpId` und `origins`; `manager.dataDir`; `manager.bind`; `manager.bootstrap: true` für den ersten Start. |
| Hosts einbinden, Agenten bereitstellen | `manager.agentTransport` (`enrollListen`, `managerUrl`); `settings.installationId`, `settings.agentImage`, `settings.volumeBase`. |
| Zustand, Ausfallereignisse | `agentTransport.monitorUser` und `monitorSecret`; `settings.accounts`. |
| Cluster anlegen | Alles Obige plus `accounts`; Netzwerke und Secrets mit der Installations-ID als Label. |
| Backups, Wiederherstellungen | `manager.backup.backendConfigFile`; `settings.backup` (Job-Image, Backend-Secret, Zugangs-Secret, Recipient-Key, Inspektionsnetz, Key-Secret). |
| Mehrere Replikate | `peers` auf jedem Replikat, `keyring`, `peerTLS`; siehe Betriebshandbuch. Drei normale Manager-Prozesse liefen auf Labor-VMs (E4 B2); der HA-Stack nicht [Ungetestet]. |

## Beispiel: Einzelinstanz

```json
{
  "auth": { "enabled": true, "rpId": "db.example.com", "rpDisplayName": "cluster-manager", "origins": ["https://db.example.com"] },
  "manager": {
    "nodeId": "manager-1",
    "dataDir": "/var/lib/dbcm-manager",
    "bind": "127.0.0.1:7946",
    "bootstrap": true,
    "autoResume": true,
    "agentTransport": {
      "enrollListen": "0.0.0.0:7444",
      "managerUrl": "https://10.0.0.4:7444",
      "monitorUser": "dbcm_monitor",
      "monitorSecret": "dbcm-inst-monitor-pw"
    },
    "backup": { "backendConfigFile": "/etc/dbcm/backend.json" },
    "settings": {
      "installationId": "inst-1",
      "volumeBase": "/var/lib/dbcm",
      "agentImage": "registry.example.com/dbcm-agent:0.0.0-dev",
      "accounts": { "monitorUser": "dbcm_monitor", "monitorSecret": "dbcm-inst-monitor-pw",
                    "backupUser": "dbcm_backup", "backupSecret": "dbcm-inst-backup-pw", "backupLogical": true },
      "backup": { "jobImage": "registry.example.com/dbcm-jobs:0.0.0-dev", "backendSecret": "dbcm-inst-backup-backend",
                  "dbCredentialSecret": "dbcm-inst-backup-pw", "recipientKey": "<öffentlicher Recovery-Key>",
                  "keySecret": "dbcm-inst-recovery-key", "inspectionNetwork": "dbcm-inspect" }
    }
  }
}
```

Alle Namen oben sind Beispiele. Die Secrets und Netzwerke müssen im Swarm existieren und das Label `dbcm.installation=inst-1` tragen.

## Beispiel: Replikat-Ausschnitt mit Schlüsselring und Peer-TLS

Für Replikat `manager-2` von dreien: Nach `manager -issue-peer-certs ./peers -installation inst-1 -peer-ids manager-1,manager-2,manager-3` nur sein `peer-bundle.pem` ins lokale Daten-Volume kopieren und versiegelte CA und Passphrase als getrennte Online-Secrets einbinden. Siehe [HA-Installation](../../INSTALL.md#three-replica-ha-stack-static-checks-and-in-process-tests-only):

```json
"manager": {
  "nodeId": "manager-2",
  "dataDir": "/var/lib/dbcm",
  "bind": "10.0.0.5:7445",
  "advertise": "10.0.0.5:7445",
  "apiForwardBind": "10.0.0.5:7447",
  "peers": [ {"id": "manager-1", "address": "10.0.0.4:7445", "apiAddress": "10.0.0.4:7447"},
             {"id": "manager-2", "address": "10.0.0.5:7445", "apiAddress": "10.0.0.5:7447"},
             {"id": "manager-3", "address": "10.0.0.6:7445", "apiAddress": "10.0.0.6:7447"} ],
  "bootstrap": true,
  "keyring": true,
  "peerTLS": { "installation": "inst-1", "caFile": "/run/secrets/dbcm-peer-ca.pem",
               "certFile": "/var/lib/dbcm/peer/peer-bundle.pem",
               "keyFile": "/var/lib/dbcm/peer/peer-bundle.pem",
               "renew": { "caFile": "/run/secrets/dbcm-peer-ca-sealed",
                          "passphraseFile": "/run/secrets/dbcm-peer-ca-passphrase" } }
}
```

Das Registrierungsverfahren für den Schlüsselring steht im [Betriebshandbuch](11-operator-runbook.md#5-replikate-schlüsselring-und-peer-zertifikate). Jedes Replikat braucht außerdem einen lokalen Docker-Socket eines Swarm-Managers (siehe [Bekannte Grenzen](10-known-limits.md)).

### `manager.tls` und `manager.tls.acme`

Für API und Oberfläche implementiert [Fakes], unabhängig von `peerTLS` und `agentTransport`. Einrichtung und HA-Voraussetzungen: [Manager-HTTPS und ACME](17-manager-tls.md). HTTP ist standardmäßig auf numerisches Loopback beschränkt; exponiertes HTTP braucht eine ausdrückliche Freigabe. Auch der Prototyp-Standard im Container bindet jetzt Loopback.

| Feld | Standard | Bedeutung |
| --- | --- | --- |
| `tls.certFile`, `tls.keyFile` | fehlt | Importierte PEM-Zertifikatskette und privater Schlüssel; beide gemeinsam nötig, beim Start geladen. Schließt ACME aus. |
| `tls.allowPlainHttp` | `false` | Explizite Freigabe für exponiertes HTTP, für geschützte Listener hinter Ihrem TLS-Proxy. Schließt Zertifikatseinstellungen aus. |
| `tls.acme` | fehlt | Automatisches HTTPS aktivieren und CA-Bedingungen akzeptieren; braucht verwalteten Modus. |
| `tls.acme.directoryUrl` | `https://acme-staging-v02.api.letsencrypt.org/directory` | HTTPS-RFC-8555-Verzeichnis. Ausdrücklich angeben; Produktion braucht eine explizite Produktions-URL. |
| `tls.acme.email` | Pflicht | Reine Kontakt-E-Mail-Adresse des Kontos. |
| `tls.acme.domains` | Pflicht | Nicht leere Liste eindeutiger kleingeschriebener ASCII-/Punycode-DNS-Namen, ohne IPs oder Wildcards. Auf allen Replikaten identisch. |
| `tls.acme.challenge` | `tls-alpn-01` | `tls-alpn-01` (öffentlicher Port 443 zum TLS-Listener) oder `http-01` (öffentlicher Port 80 zum Challenge-Listener). |
| `tls.acme.httpListen` | `:80` | Separater HTTP-Listener nur für HTTP-01. Beim ausgelieferten Container hohen Port und externe Weiterleitung nutzen. |

API und Oberfläche nutzen weiter `-listen`; ACME ändert dessen Standardport 8080 nicht. Nur der Leader erneuert (nach Wahl und stündlich, ab 30 Tagen Restlaufzeit; fünf Minuten nach Fehlern). Zertifikate, Kontoschlüssel und Challenges liegen verschlüsselt im replizierten Metastore. Alle Replikate brauchen Zugriff auf die Datenschlüssel. `-healthcheck` liest dieselbe Konfiguration und nutzt geprüftes HTTPS mit der ersten ACME-Domain als SNI; statisches TLS nutzt `auth.rpId` als SNI, sonst die Loopback-Adresse. Staging/private CAs brauchen eine installierte Vertrauenswurzel (`SSL_CERT_FILE` wird von Go unterstützt); die Prüfung lässt sich nicht umgehen.

## Gemeinsame Schreibknoten-Wahl bei redundanten Proxys (E8-Folgearbeit)

**[Nur Fakes, 25.10.2026]** E8 zeigte: Zwei unabhängige Proxys mit `single-writer-stable` können verschiedene Schreibknoten wählen. Für redundante Schreib-Endpunkte muss die gemeinsame, replizierte Wahl eingerichtet werden. Jeder Manager erhält denselben Eintrag im Abschnitt `manager`:

```json
"proxyWriters": [{
  "clusterId": "cluster-1", "initialWriter": "n1", "candidates": ["n1", "n2", "n3"], "autoFailover": true,
  "members": [
    {"id": "proxy-a", "tokenFile": "/run/secrets/proxy-a-token"},
    {"id": "proxy-b", "tokenFile": "/run/secrets/proxy-b-token"}
  ],
  "adminTokenFile": "/run/secrets/proxy-writer-admin-token"
}]
```

Jeder Proxy und die Administration brauchen unterschiedliche, zufällige Geheimnisse. Die Dateien werden außerhalb der Versionsverwaltung auf allen Manager-Replikaten bereitgestellt; die Pfade müssen absolut sein. `candidates` enthält die zugelassenen, als Writer geeigneten Knoten-IDs. Mitglieder- und Kandidatenliste bleiben nach der ersten gespeicherten Wahl fest; eine Abweichung sperrt die Wahl. Jeder Proxy erhält neben `policy` und `probe` einen eigenen `coordination`-Abschnitt. Die Knoten-IDs müssen übereinstimmen:

```json
"policy": {"mode": "single-writer-stable"},
"coordination": {
  "cluster_id": "cluster-1", "proxy_id": "proxy-a",
  "manager_urls": ["https://manager-1.example.test", "https://manager-2.example.test"],
  "token_file": "/run/secrets/proxy-a-token", "timeout": "750ms"
}
```

Der Proxy fragt pro `probe.interval` beim Manager-Leader an. Alle Manager-API-Adressen werden ohne abschließenden Schrägstrich eingetragen. HTTPS ist Pflicht; nur Tests auf Loopback dürfen HTTP verwenden. Der koordinierte Modus verlangt `probe.sync: galera`; der optionale Lese-Endpunkt ist gesperrt, weil dessen L4-Verbindung SQL-Schreibzugriffe nicht verhindern kann. Ist der Manager nicht erreichbar, behält ein laufender Proxy seine letzte Wahl, solange die lokale Galera-Prüfung den Knoten freigibt. Ein neu gestarteter Proxy hat bis zur Antwort des Leaders keinen Schreibknoten. Fällt der gewählte Knoten aus, gibt es **keinen Schreibknoten**, bis der Manager einen koordinierten Wechsel abgeschlossen hat; eine lokale Ersatzwahl findet nicht statt. Mit `autoFailover: true` prüft der Leader frische Primary/Synced-Daten und sperrt deterministisch den Wechsel zum ersten gesunden Kandidaten. Er startet keinen Cluster neu, und eine fehlende Proxy-Bestätigung blockiert den Abschluss. Beim Standardwert `false` prüft die Administration einen gesunden Galera-Kandidaten und fordert `POST /api/v1/proxy-writers/cluster-1/switch` mit `Authorization: Bearer <admin-token>` und `{"writer":"n2"}` an. In der Sperrphase beenden alle Proxys ihre Schreibverbindungen und bestätigen dies; erst danach gibt der Manager n2 frei. Ein nicht erreichbares Mitglied blockiert den Wechsel und damit Schreibzugriffe. Ein Mitglied darf nicht bloß wegen einer Partition entfernt werden. Prüfen Sie `/status` jedes Proxys (`coordination.epoch`, `coordination.pending`, `writer`) sowie die Wahl am Manager. Siehe [ADR 0017](../../decisions/0017-coordinated-proxy-writer.md).
