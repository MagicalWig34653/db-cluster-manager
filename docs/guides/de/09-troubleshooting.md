---
title: Fehlersuche
---

# Fehlersuche

[Übersicht](index.md) | Zurück: [Sicherheitsmodell](08-security-model.md) | Weiter: [Bekannte Grenzen](10-known-limits.md)

Einträge mit „im Labor gesehen“ wurden in den Experimenten E4-E6 beobachtet (siehe `tests/p0/results/`); die übrigen sind aus Code oder API-Vertrag abgeleitet.

| Symptom | Wahrscheinliche Ursache | Vorgehen |
| --- | --- | --- |
| API antwortet `409 settings_incomplete` | Installations-ID, Volume-Basis, Agent-Image oder Backup-Einstellungen fehlen in den Manager-`settings` | `manager.settings` vervollständigen, neu starten. |
| Agent-Tasks `Rejected`: `bind source path does not exist: /var/lib/dbcm-agent` (im Labor gesehen) | Swarm legt Bind-Quellen nicht an | `/var/lib/dbcm-agent` und die Volume-Basis auf jedem Host anlegen. |
| `refused (not-owned): referenced network/secret ... not in allow-list` (im Labor gesehen) | Netzwerk oder Secret ohne Label `dbcm.installation=<Installations-ID>`, oder der Agent-Dienst entstand vor der Aufnahme des Secrets in `agentTransport.dbSecrets` | Label setzen, Secret eintragen, Agenten neu ausrollen. |
| Dienstbefehle auf einem Worker scheitern: `This node is not a swarm manager` | Kein eingebundener Manager-Knoten mit Agent | Swarm-Manager-Knoten einbinden und dort den Agenten ausrollen. |
| Pull-Fehler, Tasks bleiben `preparing` | Image fehlt auf einem Knoten, keine Registry | Image auf jedem Knoten laden oder ziehen. |
| Einrichtungstoken verloren | Token nur beim Start ausgegeben | Neu starten, solange kein Administrator existiert; neues Token aus dem Log lesen. |
| Kein Passkey-Dialog oder Origin-Fehler | `rpId` oder `origins` passen nicht zur verwendeten HTTPS-URL | Konfiguration korrigieren; Passkeys brauchen den exakten HTTPS-Origin. |
| Alle Passkeys verloren | - | Wiederherstellungscode einlösen (`/auth/recovery/code`), dann neuen Passkey registrieren. Einen Befehl zum Ausstellen von Wiederherstellungs-URLs gibt es noch nicht. |
| API antwortet 503 mit `X-DCM-Leader` | Dieses Replikat erreicht keinen aktiven Leader oder die Weiterleitung ist nicht konfiguriert | Leader, Peer-mTLS und Port 7447 prüfen; vor dem Wiederholen eines unklaren Schreibvorgangs dessen Zustand prüfen. Drei normale Manager-Prozesse liefen auf Labor-VMs; HA-Stack und Weiterleitungs-Listener noch nicht. Führt kein Replikat und das Log meldet, den Datenschlüssel nicht entpacken zu können, ist das Replikat noch nicht im Schlüsselring registriert: siehe [Betriebshandbuch](11-operator-runbook.md#5-replikate-schlüsselring-und-peer-zertifikate). |
| Operation `paused` oder ein Schritt pollt dauernd | Ungeklärte Vorbedingung (z. B. Knoten nicht `Synced`) | `progress` in `GET /api/v1/operations` lesen (die Seite Vorgänge zeigt den Wartegrund und in Rot den letzten Fehler des Schritts), Ursache beheben, dann `POST /api/v1/operations/{id}/resume`. Abbruch greift beim nächsten Poll oder Schrittwechsel (`.../abort`). |
| `409 busy` oder `resource locked` | Eine andere Operation hält den Cluster | Warten oder die unfertige/fehlgeschlagene Operation zuerst abbrechen. |
| `409 requires_guided_recovery` beim Ersetzen | Quorum oder geprüfter Donor nicht beobachtet | Weg der [Totalausfall-Wiederherstellung](07-full-outage-recovery.md); keine Übersteuerung. |
| `403` bei der Freigabe einer Totalausfall-Wiederherstellung | Die Berechtigung `recover` hat nur das Administrator-Preset | Als Administrator freigeben. |
| Replikate erreichen sich mit `peerTLS` nicht | Falsche Installations-ID, falsche Replikat-ID zu einer Adresse, Zertifikat einer anderen CA oder gesperrte Seriennummer | `installation`, `peers[].id` und die Zertifikatsidentität `dbcm://<Installation>/metastore/<id>` vergleichen; mit `-issue-peer-certs` neu ausstellen. |
| `409` beim Eintritt in die Host-Wartung | Ein Cluster würde außerhalb des Hosts die Mehrheit verlieren, oder eine Operation ist unfertig | `GET /api/v1/hosts/{id}/maintenance-plan` lesen; nichts wurde geändert. |
| `409 approval_mismatch` | Digest weicht vom gespeicherten Plan ab | `approval.digest` der Operation erneut lesen und diesen freigeben. |
| Erstellung fehlgeschlagen, Name belegt | Fehlgeschlagene Erstellung behält Name und Kapazität | Cluster löschen (Freigabe, nur Administrator), siehe [Cluster anlegen](03-create-cluster.md). |
| Backup scheitert mit `docker-error` auf einem Worker | Job von einem Nicht-Manager-Agenten angelegt (im Labor gesehen, behoben) | Sicherstellen, dass ein Agent auf einem Manager-Knoten eingebunden ist. |
| Backup-Warnung `galera_info not found` | MariaDB 11.4 schreibt sie nicht nach `--extra-lsndir` (im Labor gesehen) | Nur Hinweis; die Position wird ggf. aus dem Stream gelesen (nur Fakes). |
| Proxy: etwa zwei abgebrochene Handshakes pro Sekunde und Knoten in den Knoten-Logs | Health-Proben des Proxys | Erwartet; je eine Logzeile (E5). |
| Nach Ausfall des Schreibknotens etwa 6 s kein Schreiben möglich | Galeras Ausfallerkennung (E5) | Erwartet; der Proxy selbst wechselte binnen 0,2 s. |
| Ein Backup bleibt im Schritt `wait-backup` mit `manifest not written yet`, der Job-Dienst `dbcm-job-...` zeigt `0/1` | Der Job ist beendet und konnte seinen Status nicht schreiben (Ziel nicht erreichbar, im Labor E14 gesehen); die Operation hat kein Signal für einen toten Job | `docker service logs dbcm-job-<id>` auf dem Manager-Knoten zeigt die Ursache. Ziel reparieren, Operation abbrechen, neues Backup starten. |
| Ein Backup wartet mit `backup job failed: ... 403 SignatureDoesNotMatch` oder `507 XMinioStorageFull` | Falsche S3-Zugangsdaten oder volles Ziel (im Labor E14 gesehen) | Der Grund nennt die Ursache. Backend-Secret korrigieren oder Platz schaffen, Operation abbrechen, neu starten. |
| Ein abgebrochenes Backup ließ den Dienst `dbcm-job-...` mit `0/1` zurück (Labor E14) | Ältere Versionen entfernten den Job beim Abbrechen nicht | Seit 2026-10-31 startet das Abbrechen eine Aufräum-Operation (`dbbackup/job-cleanup`), die die Job-Dienste des abgebrochenen Backups entfernt [Fakes]. Bei älteren Versionen nach dem Lesen der Logs von Hand entfernen: `docker service rm dbcm-job-<id>` (nur dieser Name). |
| Redundante Proxys zeigen `writer: none` und `coordination.pending` | Ein gemeinsamer Wechsel ist gesperrt, bis alle eingetragenen Proxys bestätigt haben | Prüfen Sie `/status` jedes Proxys, die Manager-Verbindung und Galera-Health. Stellen Sie den fehlenden Proxy wieder her; entfernen Sie seine Mitgliedschaft nicht bloß wegen einer Partition. |
| `429` oder „Zu viele Versuche“ bei der Anmeldung | Der Limiter je Adresse: 5 nicht abgeschlossene oder fehlgeschlagene Anmeldeversuche in 15 Minuten sperren die Adresse für 15 Minuten (bis 24 Stunden verdoppelt). Abgeschlossene Anmeldungen setzen den Zähler zurück | Die Oberfläche zeigt die Restzeit. Abwarten, nicht in einer Schleife wiederholen (jeder Versuch während der Sperre wird abgewiesen). Hinter einem Reverse Proxy teilen sich alle Nutzer die Proxy-Adresse. |
| Abbruch eines Rolling Restarts, Minor-Upgrades oder Scale-in antwortet `409` | Die Operation hat einen Datenbankdienst gestoppt (auf 0 skaliert) und noch nicht wieder gestartet; ein Abbruch ließe den Knoten gestoppt (im Labor E13 gesehen) | Operation fortsetzen, damit sie den Knoten startet, oder den Knoten von Hand starten (`docker service scale <Dienst>=1` auf dem Manager-Knoten), dann abbrechen. |
| Eine Operation steht im Zustand `unknown` | Ihr Start wurde abgelehnt, bevor etwas lief (z. B. hielt eine andere Operation die Sperre) | Auf der Seite Vorgänge abbrechen; das schließt nur den Eintrag. |

## Belege sammeln

Manager-Log (stderr oder `docker logs`), `GET /api/v1/operations`, `GET /api/v1/events`, `GET /api/v1/clusters/{id}` und auf einem Knoten `docker service ps --no-trunc <Dienst>`. Kein `docker system prune`, keine Volumes von Hand löschen: Der Identitätsmarker eines Volumes schützt davor, auf fremden Daten zu starten.
