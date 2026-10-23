---
title: Konfigurationsreferenz
---

# Konfigurationsreferenz

[Übersicht](index.md) | Zurück: [Betriebshandbuch](11-operator-runbook.md)

Abgeleitet aus `cmd/manager/main.go`, `cmd/manager/managed.go` und `cmd/manager/peers.go` (Stand 2026-10-23). Der Manager liest eine JSON-Datei, angegeben mit `-config <Datei>` oder der Umgebungsvariable `DCM_CONFIG`. **Unbekannte Felder werden abgelehnt**, der Manager startet dann nicht. Echtes JSON kennt keine Kommentare; die `//`-Hinweise unten dienen nur der Erklärung.

Prüfstufe: Parsen und Validierung sind unit-getestet [Fakes]. Die Abschnitte `auth`, `manager`, `manager.agentTransport`, `manager.backup` und `manager.settings` liefen in der Konfiguration von E4-E6 auf Lab-VMs (eine Instanz als Prozess auf einem Swarm-Manager-Knoten) [Lab-VMs]. `manager.keyring`, `manager.peerTLS`, mehrere `peers`, `-print-replica-key` und `-issue-peer-certs` wurden nur mit Fakes und Loopback-TLS getestet [Fakes]; ein Multi-Replika-Manager lief nie auf VMs.

## Kommandozeile

| Option | Standard | Bedeutung |
| --- | --- | --- |
| `-listen addr` | `127.0.0.1:8080` | Adresse von UI und API. Kein TLS: einen TLS-Reverse-Proxy davorsetzen. |
| `-config file` | `$DCM_CONFIG` | JSON-Konfiguration. Ohne sie läuft der Manager im Prototyp-Modus. |
| `-version` | | Version ausgeben und beenden. |
| `-healthcheck` | | `http://127.0.0.1:<Port von -listen>/api/v1/health` abfragen, bei Gesundheit Exit 0 (Container-`HEALTHCHECK`). |
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
{ "auth": { "enabled": true, "rpId": "db.example.com", "rpDisplayName": "DB Cluster Manager", "origins": ["https://db.example.com"] } }
```

## `manager`

Der Abschnitt wählt den verwalteten Modus. Pflicht: `dataDir` und `bind`.

| Feld | Typ | Standard | Bedeutung |
| --- | --- | --- | --- |
| `nodeId` | string | `manager-1` | ID dieses Replikats. Muss zum Eintrag in `peers` und, bei `peerTLS`, zur Zertifikatsidentität passen. |
| `dataDir` | string | keiner, **Pflicht** | Metastore (`<dataDir>/metastore`), `data.key`, `replica.key` und der Standard-Monitoringspeicher. Sichern, siehe [Betriebshandbuch](11-operator-runbook.md). |
| `bind` | string | keiner, **Pflicht** | TCP-Adresse des Metastores, zum Beispiel `127.0.0.1:7946`. Ohne `peerTLS` ist der Transport unverschlüsseltes TCP: nur Loopback oder isolierte Netze. |
| `advertise` | string | leer | Adresse, die Peers anwählen, falls sie von `bind` abweicht. |
| `peers` | Liste von `{id, address}` | leer | Anfängliche stimmberechtigte Mitgliedschaft **einschließlich dieses Replikats**; leer bedeutet Einzelinstanz. Danach gibt es keine Mitgliedschaftsänderung. |
| `bootstrap` | bool | `false` | Schreibt beim ersten Start einer neuen Installation die Anfangsmitgliedschaft (`peers`); wird ignoriert, wenn das Replikat schon Zustand auf Platte hat. Alle bootstrappenden Replikate müssen dieselbe Liste übergeben, also bei einer neuen Installation auf jedem Replikat `true`. |
| `keyFile` | string | `<dataDir>/data.key` | 32-Byte-Datenschlüssel für Geheimnisse im Ruhezustand. Ohne `keyring` wird er bei Bedarf angelegt und muss von Hand auf jedes Replikat kopiert werden. Mit `keyring` wird eine vorhandene Datei beim Bootstrap des Schlüsselrings als erster Schlüssel übernommen; eine neue wird nicht angelegt. |
| `keyring` | bool | `false` | Replizierter, verpackter Datenschlüsselring ([ADR 0013](../../decisions/0013-replica-keys-peer-tls-and-revocation.md)). Jedes Replikat hält nur seine Identität in `<dataDir>/replica.key`. Empfohlen bei mehr als einem Replikat. |
| `peerTLS` | Objekt | fehlt | Gegenseitiges TLS 1.3 zwischen Metastore-Replikaten, siehe unten. Empfohlen bei mehr als einem Replikat. |
| `labAgents` | bool | `false` | Unauthentifizierte HTTP-Agentenendpunkte des Labors. **Nur Labor**; schließt `agentTransport` aus. Ohne beides werden Agentenbefehle abgelehnt. |
| `autoResume` | bool | `false` | Ein neuer Leader setzt übernommene Operationen fort. |
| `agentTransport` | Objekt | fehlt | Authentifizierter mTLS-Agententransport mit Einbindung ([ADR 0010](../../decisions/0010-agent-transport-and-enrollment.md)). Praktisch nötig für Hosts, Cluster und Backups. |
| `monitoring` | Objekt | aktiv | Metrikspeicher und Alarme. |
| `backup` | Objekt | fehlt | Manager-Seite der Cluster-Backups. |
| `settings` | Objekt | fehlt | Installationseinstellungen, die der Leader bei jedem Leadership-Start in die replizierten Einstellungen einmischt (noch keine API dafür). |

### `manager.peers[]`

| Feld | Bedeutung |
| --- | --- |
| `id` | Replikat-ID, gleich dem `nodeId` dieses Replikats. |
| `address` | Metastore-Adresse dieses Replikats, wie die anderen sie anwählen. Mit `peerTLS` muss das Replikat unter dieser Adresse genau diese ID vorweisen. |

### `manager.peerTLS`

Existiert der Abschnitt, sind alle vier Felder Pflicht. Erzeugt von `manager -issue-peer-certs`. Die Zertifikatsdatei wird neu gelesen, wenn sie sich ändert; ein neu ausgestelltes Zertifikat braucht keinen Neustart. Die Peers müssen in `peers` stehen; die Zertifikatsidentität ist `dbcm://<Installation>/metastore/<Replikat-ID>`.

| Feld | Bedeutung |
| --- | --- |
| `installation` | Installations-ID, auf jedem Replikat gleich und gleich `settings.installationId`. |
| `caFile` | Zertifikat der Peer-CA (`ca.pem`). |
| `certFile` | Kette dieses Replikats (`<id>/peer.pem`). |
| `keyFile` | Privater Schlüssel dieses Replikats (`<id>/peer-key.pem`). |

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

## Was der verwaltete Modus braucht

| Ziel | Nötige Konfiguration |
| --- | --- |
| Verwalteten Modus starten | `auth.enabled` mit `rpId` und `origins`; `manager.dataDir`; `manager.bind`; `manager.bootstrap: true` für den ersten Start. |
| Hosts einbinden, Agenten bereitstellen | `manager.agentTransport` (`enrollListen`, `managerUrl`); `settings.installationId`, `settings.agentImage`, `settings.volumeBase`. |
| Zustand, Ausfallereignisse | `agentTransport.monitorUser` und `monitorSecret`; `settings.accounts`. |
| Cluster anlegen | Alles Obige plus `accounts`; Netzwerke und Secrets mit der Installations-ID als Label. |
| Backups, Wiederherstellungen | `manager.backup.backendConfigFile`; `settings.backup` (Job-Image, Backend-Secret, Zugangs-Secret, Recipient-Key, Inspektionsnetz, Key-Secret). |
| Mehrere Replikate | `peers` auf jedem Replikat, `keyring`, `peerTLS`; siehe Betriebshandbuch. Ein Mehrreplikat-Lauf auf VMs fand nicht statt [Ungetestet]. |

## Beispiel: Einzelinstanz

```json
{
  "auth": { "enabled": true, "rpId": "db.example.com", "rpDisplayName": "DB Cluster Manager", "origins": ["https://db.example.com"] },
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

Für Replikat `manager-2` von dreien, nach `manager -issue-peer-certs ./peers -installation inst-1 -peer-ids manager-1,manager-2,manager-3` und dem Kopieren von `./peers/manager-2/` auf diesen Host (die versiegelte CA `ca.sealed` und die Passphrase bleiben offline):

```json
"manager": {
  "nodeId": "manager-2",
  "dataDir": "/var/lib/dbcm-manager",
  "bind": "0.0.0.0:7946",
  "advertise": "10.0.0.5:7946",
  "peers": [ {"id": "manager-1", "address": "10.0.0.4:7946"},
             {"id": "manager-2", "address": "10.0.0.5:7946"},
             {"id": "manager-3", "address": "10.0.0.6:7946"} ],
  "bootstrap": true,
  "keyring": true,
  "peerTLS": { "installation": "inst-1", "caFile": "/etc/dbcm/peer/ca.pem",
               "certFile": "/etc/dbcm/peer/peer.pem", "keyFile": "/etc/dbcm/peer/peer-key.pem" }
}
```

Das Registrierungsverfahren für den Schlüsselring steht im [Betriebshandbuch](11-operator-runbook.md#5-replikate-schlüsselring-und-peer-zertifikate). Jedes Replikat braucht außerdem einen lokalen Docker-Socket eines Swarm-Managers (siehe [Bekannte Grenzen](10-known-limits.md)).
