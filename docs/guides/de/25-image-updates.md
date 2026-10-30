---
title: Image-Updates
---

# Image-Updates [Nur mit Fakes geprüft; nicht gegen Registry oder Swarm]

Der Image-Update-Dienst ([ADR 0025](../../decisions/0025-image-update-service.md), Anforderung X16) findet neuere Images für Ihre verwalteten Dienste und aktualisiert sie nur dort, wo Sie es erlauben: neueren Tag oder Digest ziehen, Swarm-Dienst aktualisieren, Health-Gate abwarten, bei Fehlern zurückrollen. Er arbeitet wie Watchtower, aber nur auf Objekten, die der Manager besitzt, und immer über das genehmigungspflichtige Dienst-Update des Managers. Dieses Modul behandelt **ausschließlich Container-Images**. Die Docker-Engine (docker-ce, containerd, buildx- und compose-Plugins) wird so nie aktualisiert; sie gehört zur [Patch-Verwaltung](23-patch-management.md) als standardmäßig nur berichtende Paketklasse `container-runtime` ([ADR 0023](../../decisions/0023-os-patch-management.md)).

## Aktivieren

Standardmäßig aus. In der Manager-Konfiguration ergänzen (braucht `auth.enabled`):

```json
"imageUpdate": { "enabled": true, "pollIntervalSeconds": 300 }
```

Auch aktiviert startet jeder Dienst im Modus `manual`: Ohne Richtlinie eines Administrators wird nichts zeitgesteuert geprüft.

## Was im Geltungsbereich liegt

Nur Dienste mit dem Besitz-Label Ihrer Installation. Fremde Container und Dienste anderer Installationen sind unsichtbar und liefern 404. Ein fremder Dienst kann mit dem Label `dbcm.image-update=opt-in` teilnehmen; er wird dann nur gemeldet (kein Genehmigen oder Aktualisieren).

## Richtlinien pro Dienst

`PUT /api/v1/image-updates/services/{id}/policy` (Administrator):

| Feld | Bedeutung |
| --- | --- |
| `mode` | `manual` (Standard), `notify` (zeitgesteuerte Prüfung und geprüfter Plan, Sie genehmigen), `auto` (geeignete Pläne genehmigt die Richtlinie selbst und wendet sie im Fenster an) |
| `track` | `digest` (gleicher Tag, neuer Digest), `patch`, `minor`, `major` |
| `windows` | Wartungsfenster (Tage, `start` HH:MM, `durationMinutes`, `zone`); für `auto` Pflicht; gilt auch beim manuellen Anwenden |
| `strategy` | Swarm-Update-Strategie: `order`, `parallelism`, `delaySeconds`, `monitorSeconds`, `maxFailureRatio`; Fehler bedeutet immer Rollback |
| `checkEveryMinutes` | Standard 360 |
| `waitMinutes` | wie lange ein Update laufen darf, bevor das Ergebnis „unbekannt“ ist (Standard 15) |

Feste Regeln: Datenbanken folgen nie `major`; `auto` überschreitet nie eine Hauptversion, wendet nie einen Minor-Sprung einer Datenbank an und berührt nie Galera, selbst gebaute Images, Rollbacks oder fremde Dienste. Galera-Cluster erhalten Pläne, die nur den Kandidaten zeigen; das Update läuft über den Rolling- bzw. Minor-Upgrade-Planer.

## Ablauf

1. `POST .../services/{id}/check` (Operator) oder der Scheduler liest die Registry (Zugangsdaten aus dem versiegelten Registry-Speicher) und erstellt einen Plan: altes Image, neues Image `repo:tag@sha256:...`, Art, Strategie, Digest. Die erste Prüfung eines nicht gepinnten Tags speichert nur den Digest-Ausgangswert.
2. Sie prüfen und genehmigen mit dem Plan-Digest: `POST /image-updates/plans/{id}/approve`.
3. `POST .../apply` startet das Dienst-Update mit Swarm `UpdateConfig`/`RollbackConfig`, genehmigt es mit Ihrer Berechtigung und wartet. Ergebnisse: `succeeded`, `rolled_back` (Swarm hat die vorherige Spezifikation wiederhergestellt; mit Grund vermerkt), `failed` (auch unbekanntes Ergebnis nach der Wartezeit).
4. `POST .../rollback` auf einem erfolgreichen Plan schlägt den Rückweg vor; er braucht eine eigene Genehmigung und ist nie automatisch.

Pläne halten fest, wer genehmigt hat, die ausführende Agent-Laufzeit (`executedBy`) und die Operations-ID. Benachrichtigungen: `imageupdate.candidate`, `applied`, `rolled_back`, `failed`, `check_failed`.

## Gepatchte Images aus dem Image-Build-Dienst

`POST /image-updates/offers` mit `{"jobId": "..."}` eines erfolgreichen Builds erstellt `built`-Pläne für Dienste mit demselben Repository. Sie brauchen immer einen Menschen.

## Grenzen

Nur Fakes; noch keine Oberfläche; keine Registry-Webhooks; die automatische Übergabe aus Schwachstellen-Builds ist nicht verdrahtet; der laufende Digest wird nicht aus Swarm gelesen (Ausgangswert der ersten Prüfung). Siehe ADR.
