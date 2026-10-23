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
| API antwortet 503 mit `X-DCM-Leader` | Dieses Replikat ist nicht Metastore-Leader (keine Weiterleitung) | Den Leader ansprechen. Mehrere Replikate sind auf VMs ungetestet. |
| Operation `paused` oder ein Schritt pollt dauernd | Ungeklärte Vorbedingung (z. B. Knoten nicht `Synced`) | `progress` in `GET /api/v1/operations` lesen, Ursache beheben, dann `POST /api/v1/operations/{id}/resume`. Abbruch greift beim nächsten Poll oder Schrittwechsel (`.../abort`). |
| `409 busy` oder `resource locked` | Eine andere Operation hält den Cluster | Warten oder die unfertige/fehlgeschlagene Operation zuerst abbrechen. |
| `409 requires_guided_recovery` beim Ersetzen | Quorum oder geprüfter Donor nicht beobachtet | Weg der [Totalausfall-Wiederherstellung](07-full-outage-recovery.md); keine Übersteuerung. |
| `409 approval_mismatch` | Digest weicht vom gespeicherten Plan ab | `approval.digest` der Operation erneut lesen und diesen freigeben. |
| Erstellung fehlgeschlagen, Name belegt | Fehlgeschlagene Erstellung behält Name und Kapazität | Cluster löschen (Freigabe, nur Administrator), siehe [Cluster anlegen](03-create-cluster.md). |
| Backup scheitert mit `docker-error` auf einem Worker | Job von einem Nicht-Manager-Agenten angelegt (im Labor gesehen, behoben) | Sicherstellen, dass ein Agent auf einem Manager-Knoten eingebunden ist. |
| Backup-Warnung `galera_info not found` | MariaDB 11.4 schreibt sie nicht nach `--extra-lsndir` (im Labor gesehen) | Nur Hinweis; die Position wird ggf. aus dem Stream gelesen (nur Fakes). |
| Proxy: etwa zwei abgebrochene Handshakes pro Sekunde und Knoten in den Knoten-Logs | Health-Proben des Proxys | Erwartet; je eine Logzeile (E5). |
| Nach Ausfall des Schreibknotens etwa 6 s kein Schreiben möglich | Galeras Ausfallerkennung (E5) | Erwartet; der Proxy selbst wechselte binnen 0,2 s. |

## Belege sammeln

Manager-Log (stderr oder `docker logs`), `GET /api/v1/operations`, `GET /api/v1/events`, `GET /api/v1/clusters/{id}` und auf einem Knoten `docker service ps --no-trunc <Dienst>`. Kein `docker system prune`, keine Volumes von Hand löschen: Der Identitätsmarker eines Volumes schützt davor, auf fremden Daten zu starten.
