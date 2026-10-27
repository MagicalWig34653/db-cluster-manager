---
title: Agent-Laufzeiten und Fähigkeiten
---

# Agent-Laufzeiten und Fähigkeiten [Nur mit Fakes und Loopback geprüft; nicht auf VMs]

Es gibt zwei Arten von Agenten ([ADR 0024](../../decisions/0024-agent-runtimes.md)):

| Laufzeit | Was es ist | Fähigkeiten |
| --- | --- | --- |
| `container` | Der Docker-Agent, ein globaler Swarm-Dienst. Standard für alles, was Swarm-Objekte berührt. | `docker-objects`, `galera-steps`, `restic` (nur mit erlaubten Pfaden), `diagnostics` (reserviert) |
| `native` | Ein systemd-Dienst auf dem Host ohne Docker-Zugriff ([Anleitung 23](23-patch-management.md)). | `patch`, `restic` (nur mit erlaubten Pfaden), `host-maintenance` und `diagnostics` (reserviert, ungenutzt) |

Meldet ein Agent eine Fähigkeit, die seine Laufzeit nicht haben kann (etwa ein Container-Agent mit `patch`), wird die Meldung abgelehnt: beim Enrollment mit HTTP 400 `runtime-mismatch`, danach zeigt der Host `agentReportError` und wird für jede Operation gesperrt, bis eine korrekte Meldung eintrifft.

## Welche Laufzeit was ausführt

| Schritttyp | Fähigkeit | Laufzeit |
| --- | --- | --- |
| Dienste, Volumes, Configs, Secrets, Backup-Jobs, Sperrliste (`create-service`, `remove-volume`, ...) | `docker-objects` | nur Container |
| Identitätsmarker, Volume-Verzeichnisse, Knotenzustand, SQL-Vorlagen, Recovery-Schritte (`init-identity`, `exec-sql`, `collect-health`, ...) | `galera-steps` | nur Container |
| `restic-backup`, `-snapshots`, `-forget`, `-check`, `-restore`, `-stats` | `restic` | bevorzugt nativ, sonst Container |
| `patch/report`, `simulate`, `apply`, `reboot`, `deny-list` | `patch` | nur nativ |

Die Tabelle steht in `internal/agentrt`. Der Manager weicht nie stillschweigend aus: Meldet der Host die benötigte Laufzeit nicht, wird die Operation mit Begründung abgelehnt („capability patch needs a native agent; the host reports container [...]“). `POST /api/v1/host-sources/{id}/backups` antwortet dann 409 `agent_runtime_unavailable` und startet nichts. Ein Host, dessen Agent nie gemeldet hat (älterer Build), gilt als Container-Agent ohne `restic`; den Agenten aktualisieren, damit er meldet.

## Wo man es sieht

- `GET /api/v1/hosts`: `agents` listet Laufzeit und Fähigkeiten je Host; `agentReportError` erscheint nach einer inkonsistenten Meldung. Die Host-Seite zeigt ein Badge je Laufzeit (UI-Lücke: noch keine Ansicht je Funktion).
- `GET /api/v1/host-sources[/{id}]`: `runtime` ist die Laufzeit, die die Quelle jetzt ausführen würde (nativ, wenn `restic` angeboten wird, sonst Container), `runtimeNote` der Grund, wenn keine möglich ist.
- `GET /api/v1/operations`: `executedBy` nennt je Schritt `runtime`, `host` und die Agent-Identität `agent`, die ausgeführt hat. Die Zeitleiste enthält `[executed by runtime=native host=...]`.
- Patch: `agentRuntime` am Patch-Host, `executedBy` am Plan.
- Audit: ein Eintrag je ausgeführtem Schritt (`operate` auf dem Host) mit Operation, Schritt, Laufzeit und Agent-Identität.

## Restic auf dem nativen Agenten

`resticPaths` in die Installationsanfrage des Patch-Agenten aufnehmen ([Anleitung 23](23-patch-management.md)); der Plan wird wie gewohnt geprüft und freigegeben. Die Unit startet dann `dbcm-agent patch ... -restic-allow-path /srv/data -restic-secrets-dir /etc/dbcm-patch/secrets`; das Skript legt das Secret-Verzeichnis an (Modus 0700) und bricht ab, wenn `restic` fehlt. `restic` vorher aus der Distribution oder dem gepinnten Release installieren; der Preflight zeigt `restic` und `ssh` (für SFTP-Repositories). Von Hand den Agenten mit denselben Flags starten.

Es gelten die Regeln aus [Anleitung 22](22-restic.md): Der Agent sichert nur unterhalb der erlaubten Wurzeln, nie das Secret-Verzeichnis oder den Docker-Socket, liest Passwort und Backend-Zugangsdaten aus regulären Dateien im Secret-Verzeichnis, benannt nach der Quelle (`passwordSecret`, `credentialSecret`; von Hand anlegen, Modus 0600, Besitzer der Agent-Benutzer), und stellt nur in einen neuen Pfad mit Freigabe wieder her. Agenten im Report-Modus laufen als unprivilegierter Benutzer `dbcm-patch` und lesen nur, was dieser lesen darf; für Backups des ganzen Hosts den Apply-Modus (root) installieren oder dem Benutzer Leserechte auf die erlaubten Pfade geben.

## Grenzen

- Eine Identität je Host: Container-Agent und nativer Agent desselben Hosts würden gegenseitig ihr Zertifikat ablösen. Bis es Identitäten je Laufzeit gibt, nur einen von beiden je Host betreiben ([ADR 0024](../../decisions/0024-agent-runtimes.md)).
- Nur mit Fakes und Loopback-Listenern geprüft; keine VM und kein echter restic-Lauf auf dem nativen Agenten. Das Secret-Verzeichnis des nativen Agenten füllt der Manager nicht.
- `host-maintenance` und `diagnostics` sind reservierte Namen; nichts wird dorthin geroutet.
