---
title: Operator-CLI
---

# cli(1) — Kommandozeile für den Betrieb

**[Fakes]** Die Go-CLI nutzt die vorhandene REST-API unter `/api/v1`. Befehlszuordnung und Ausgabe haben Unit-Tests und Tests mit `httptest`-Servern; ein echter Manager wurde mit dieser CLI noch nicht bedient. Vertrag: [OpenAPI](../../../api/openapi.yaml). [English guide](../en/20-cli.md).

## Aufruf

```text
cli [--config PFAD] [--context NAME] [--output table|json] BEFEHL
cli help
```

Nach `make build` liegt das Programm unter `bin/cli`. Globale Optionen stehen vor dem Befehl. Standard ist eine Tabelle; `--output json` gibt die API-Antwort eingerückt aus. IDs und Digests sind normale Argumente. **Geheimnisse dürfen nie als Argument übergeben werden.**

## Kontexte und Anmeldung

```sh
cli contexts set labor https://manager.example https://manager.example
cli contexts use labor
CLI_SESSION_TOKEN_FILE=/geschuetzter/pfad/session.token cli login
cli whoami
cli contexts list
cli logout
```

Die Konfiguration liegt normalerweise im Benutzer-Konfigurationsverzeichnis unter `cluster-manager/cli.json`; `CLI_CONFIG` oder `--config` überschreibt den Pfad. Sie enthält `currentContext`, `contexts` sowie pro Kontext `url`, `origin` und optional `sessionFile`. `contexts set NAME URL ORIGIN` schreibt sie mit Modus 0600. HTTPS ist Pflicht, außer für HTTP auf `localhost`. `origin` muss dem URL-Ursprung entsprechen; schreibende API-Aufrufe benötigen den konfigurierten Origin-Header. `--context NAME` wählt einen Kontext für einen Aufruf. `logout` entfernt nur die lokale Token-Datei und widerruft die Server-Session nicht. Die serverseitige Ablaufzeit gilt weiterhin.

`cli login --token-stdin`, `cli login --token-file PFAD`, `CLI_SESSION_TOKEN` oder `CLI_SESSION_TOKEN_FILE` übernimmt ein **bereits vorhandenes, opakes Manager-Session-Token**. Die CLI prüft es mit `GET /auth/session` und legt es in einer kontextspezifischen Datei mit Modus 0600 ab. Die Umgebungsvariablen können das Token auch direkt für einzelne Befehle liefern. Operator-Sessions werden derzeit über den Passkey-Login der Weboberfläche erzeugt; die API akzeptiert für Operator-Routen nur ihren Session-Cookie. Es gibt **keinen allgemeinen API-Token-Endpunkt und keinen Bearer-Token-Login**. Die Bearer-Tokens für Proxy-Writer gelten hier nicht. Ein Token über einen geschützten Eingabekanal aus der bestehenden Passkey-Session übernehmen, niemals in Shell-Verlauf, Git-Dateien oder URLs schreiben. Auch das Verzeichnis der Session-Datei schützen.

## Befehle

| Befehl | Wirkung |
| --- | --- |
| `clusters list`, `clusters get ID` | Sichtbare Cluster auflisten oder anzeigen. |
| `clusters create --file JSON` | `POST /clusters` plant und startet das Anlegen sofort. Ein eigener Vorschau-Endpunkt existiert nicht. |
| `clusters plan ID --file JSON` | Speichert über `/clusters/{id}/operation-plan` einen Galera-Plan mit Freigabe-Digest. |
| `clusters delete ID` | Erzeugt einen Löschplan. Erst die spätere Freigabe erlaubt die Löschung. |
| `clusters approve OPERATION DIGEST` | Gibt den angezeigten Definitions-Digest frei; Alias für `operations approve`. |
| `operations list`, `operations watch ID [--interval SEKUNDEN]` | Operationen anzeigen oder bis zum Endzustand abfragen; Standardintervall zwei Sekunden. |
| `operations abort ID`, `operations approve ID DIGEST` | Abbruch an sicherer Schrittgrenze anfordern oder einen angezeigten Digest freigeben. |
| `backups list`, `backups run CLUSTER [--file JSON]` | Backup-Übersicht oder Backup-Start. Optionales JSON enthält `method` und `databases`; Standard ist physisch. |
| `backups verify CLUSTER` | Zeitpunkt der letzten *bestandenen Restore-Verifikation* prüfen; Exit 7, wenn keiner erfasst ist. Startet keinen Restore. |
| `backups verify CLUSTER --result-file PFAD [--backup ID] [--host ID]` | Startet einen isolierten Prüf-Restore. Die vollständige Antwort mit einmaligem Prüfpasswort landet in einer Datei mit Modus 0600; die Ausgabe blendet das Passwort aus. Den Operations-Digest gesondert prüfen und freigeben. Erst eine erfolgreich abgeschlossene Operation und der Verifikationszeitpunkt bestätigen das Ergebnis. |
| `hosts list` | Sichtbare Swarm-Hosts auflisten. |
| `services plan --file JSON`, `services create --file JSON` | Dienstvorschau oder Dienstanforderung; das Anlegen kann auf Freigabe warten. |
| `vuln findings list`, `vuln approve PROPOSAL DIGEST` | Schwachstellen anzeigen oder einen Vorschlag mit seinem Digest freigeben; abhängig von Konfiguration und Richtlinie. |
| `plan export --file JSON [--format json\|yaml]` | Dienstplan über `/services/plan` exportieren. Für eine Datei Shell-Umleitung verwenden. |
| `completion bash\|zsh\|fish` | Shell-Vervollständigung ausgeben. |

Eingabedateien enthalten JSON nach [OpenAPI](../../../api/openapi.yaml). Beispiel: `cli --output json clusters delete c1` liefert Operations-ID und `approval.digest`. Den Plan prüfen, dann `cli clusters approve OPERATION DIGEST` ausführen. Bei 409 ist der Digest oder Zustand veraltet; einen neuen Plan abrufen. `plan export` bezieht sich auf Dienste; für Cluster-Anlage gibt es keine eigenständige Vorschau. Gesamtausfall-Recovery und große Datenbank-Upgrades behalten ihre gesonderten Produktfreigaben.

## Rückgabewerte

| Code | Bedeutung |
| --- | --- |
| 0 | Erfolgreich; `watch` erreichte `succeeded`. |
| 1 | Sonstiger API-/Antwortfehler oder fehlgeschlagene, abgebrochene bzw. unbekannte Operation. |
| 2 | Ungültiger Aufruf, Eingabe oder lokale Konfiguration. |
| 3 | Nicht angemeldet oder nicht berechtigt, auch bei fehlender lokaler Session. |
| 4 | API-Ressource oder Backup-Übersicht fehlt. |
| 5 | Konflikt, auch bei abweichendem Freigabe-Digest. |
| 6 | API nicht verfügbar oder Netzwerkfehler. |
| 7 | Keine bestandene Restore-Verifikation erfasst. |

HTTP-Status und grober API-Fehlercode gehen nach stderr; die CLI gibt keine Server-Fehlertexte aus. Berechtigungen, Audit, Freigaben und Sicherheitsgrenzen prüft der Server.
