# Bericht zu den Review-Korrekturen

Stand: 2026-11-17. Alle acht Befunde wurden erneut am Code geprüft und **bestätigt**; keiner wurde verworfen. Das Review enthält keine Befunde der Stufe Niedrig. Je Befund wurde zuerst der Regressionstest geschrieben und danach die Implementierung geändert; jeder Befund hat einen eigenen deutschen Commit.

**Prüfgrenze:** Es wurden keine Builds, Tests, Docker-Befehle oder Netzaufrufe ausgeführt und nichts gepusht. Die Tests sind weder kompiliert noch ausgeführt; insbesondere wird kein beobachteter Rot-/Grün-Lauf behauptet. Durchgeführt wurden statische Codeprüfung, Formatierung der geänderten Go-Dateien mit `gofmt` und `git diff --check`. Die folgenden Befehle sind Arbeitsaufträge für den Aufrufer, keine Erfolgsmeldungen.

Alle Testbefehle sind aus der Repository-Wurzel auszuführen. Sie benötigen die Go-Version aus `go.mod` (Go 1.27), die Modulabhängigkeiten und für `-race` eine unterstützte C-Toolchain. Keine besonderen Umgebungsvariablen oder externen Dienste sind erforderlich. Manager-Tests starten ihre eigenen lokalen Raft-Knoten mit temporären Datenverzeichnissen und benötigen lokale Listener; SSH-Tests starten ausschließlich einen eigenen Loopback-Testserver. Keine Produktionszugänge verwenden.

## 1. Hoch – Panic bei paralleler Metrikarchivierung und Retention

**Bestätigt. Commit:** `54fb453` – `fix: Metrikarchivierung gegen parallele Retention absichern`.

`ArchiveBlocks` verwendete nach dem Entsperren veränderliche Blockzeiger und dereferenzierte `agg.path`, obwohl Retention `agg` auf `nil` setzen konnte. Der Upload verwendet jetzt kopierte Metadaten und Dateipfade. Unter derselben Store-Sperre gesetzte Leserzähler schützen die Dateien während des gesamten Upload-Snapshots; auch Fehler und Kontextabbruch lösen die Reservierungen wieder.

Regression: `TestArchiveSnapshotSurvivesRetention` löst während des ersten Uploads Retention aus, während weitere Snapshot-Einträge ihre lokalen Dateien noch benötigen. Alle ausgewählten Blöcke müssen ohne Panic und ohne verlorene Datei archiviert werden.

```sh
go test ./internal/metrics -run '^TestArchiveSnapshotSurvivesRetention$' -race -count=1
```

## 2. Hoch – Retention löscht eine gleichzeitig gepinnte Restore-Quelle

**Bestätigt. Commit:** `51bccb5` – `fix: Backup-Pins atomar mit Löschreservierungen koordinieren`.

Die vorherige Auswahl der Pins koordinierte sich nicht mit dem Inspection-Commit. Retention reserviert die Quelle nun durch ein repliziertes, versionsgeprüftes `Deleting`-Feld vor der ersten Backend-Löschung. Der Inspection-Pin und eine Versionsfortschreibung desselben Backup-Datensatzes werden atomar gespeichert. Eine veraltete Löschwahl verliert dadurch ihre Versionsprüfung; eine bereits reservierte Quelle lehnt neue Pins ab. Reservierungen überstehen Führungswechsel und bleiben auch bei geänderter Retention zur Bereinigung ausgewählt. Die physische Restore-/PITR-Auswahl überspringt reservierte Quellen.

Regression: `TestRetentionReservationAndInspectionPinAreAtomic` deckt beide Reihenfolgen ab: Pin zuerst verhindert die veraltete Reservierung; Reservierung zuerst verhindert Quellenauswahl und Pin samt zugehörigem Operations-Commit.

```sh
go test ./internal/manager -run '^TestRetentionReservationAndInspectionPinAreAtomic$' -race -count=1
```

## 3. Hoch – Destruktiver SFTP-Ersetzungsfallback

**Bestätigt. Commit:** `1ca2c57` – `fix: Destruktiven SFTP-Ersetzungsfallback entfernen`.

Nach jedem `PosixRename`-Fehler wurde bisher das Ziel gelöscht und erst danach ein gewöhnliches Rename versucht. Der destruktive Fallback ist entfernt; Veröffentlichungen verwenden ausschließlich `posix-rename`. Fehler lassen das vorhandene Ziel unangetastet, die temporäre Upload-Datei wird weiterhin bestmöglich bereinigt.

Bewusste Kompatibilitätsgrenze: Ein SFTP-Server ohne `posix-rename` kann über diesen Backend-Pfad keine Uploads veröffentlichen, auch keine neuen Objekte. Sichere Ablehnung ersetzt den bisherigen unsicheren Fallback.

Regression: `TestSFTPPublishPreservesExistingObject` injiziert Verbindungsabbruch, fehlende Erweiterungsunterstützung und einen Berechtigungsfehler. In allen Fällen dürfen weder Ziel-Löschung noch gewöhnliches Rename stattfinden. Reiner Fake-Test, kein SFTP-Dienst erforderlich.

```sh
go test ./internal/backup -run '^TestSFTPPublishPreservesExistingObject$' -race -count=1
```

## 4. Mittel – Backend-Lesefehler gelten als bestätigte Löschung

**Bestätigt. Commit:** `9ecdede` – `fix: Backup-Löschung erst nach vollständigem Erfolg bestätigen`.

Der bisherige Nachweis akzeptierte jeden Manifest-Lesefehler als Abwesenheit und betrachtete nur das erste Manifest. Die Bestätigung benötigt nun überhaupt keinen solchen Lese-Nachweis: Erst erfolgreiche Delete-Aufrufe für sämtliche abhängigen Binlogs, Manifeste, Streams, Statusobjekte und erreichbaren Kopien erlauben `DeletedAt`. Nur der bereits vorhandene explizite `backup.ErrNotFound`-Fall gilt bei Delete als Erfolg. Fehler lassen den Datensatz für einen erneuten Bereinigungsversuch offen.

Regression: `TestRetentionDoesNotConfirmIncompleteDeletion` deckt Backend-Ausfall mit gleichzeitigem Lesefehler, einen nicht gelöschten Stream und einen Fehler beim zweiten Objekt ab. Anschließend muss ein erfolgreicher Wiederholungsversuch die Löschung bestätigen.

```sh
go test ./internal/manager -run '^TestRetentionDoesNotConfirmIncompleteDeletion$' -race -count=1
```

## 5. Mittel – Logout bestätigt einen nicht dauerhaft gespeicherten Widerruf

**Bestätigt. Commit:** `26aae16` – `fix: Sitzungswiderrufe vor erfolgreichem Logout dauerhaft speichern`.

Die Sitzungsentfernung erfolgt jetzt erst nach erfolgreicher persistierter Löschung. `Revoke` reicht Fehler zurück; der Logout antwortet bei fehlgeschlagener Speicherung mit HTTP 503 und erhält die Cookies für einen erneuten Versuch. Das gilt für normale und Recovery-Sitzungen. Auch die Rotation bestätigt keinen verlorenen Widerruf des alten Tokens. Der OpenAPI-Vertrag beschreibt den Fehlerfall.

Regressionen: `TestSessionRevokeReportsPersistenceFailureAndRetries` prüft Fehlerweitergabe, Wiederholung und erneutes Laden der persistierten Sitzungen. `TestLogoutDoesNotAcknowledgeFailedRevocation` prüft HTTP 503 und den erhaltenen Cookie.

```sh
go test ./internal/recovery -run '^TestSessionRevokeReportsPersistenceFailureAndRetries$' -race -count=1
go test ./internal/api -run '^TestLogoutDoesNotAcknowledgeFailedRevocation$' -race -count=1
```

## 6. Mittel – SSH-Handshake und Sitzungseröffnung ignorieren Abbruch/Timeout

**Bestätigt. Commit:** `874f246` – `fix: SSH-Handshake und Sitzungseröffnung zeitlich begrenzen`.

Nach dem TCP-Aufbau wird die Verbindung nun bei Kontextabbruch geschlossen. Eine Socket-Deadline begrenzt Handshake und Sitzungseröffnung auf den kleineren Wert aus Runner-Timeout und Kontextdeadline. Erst nach erfolgreicher Sitzungseröffnung wird die Setup-Deadline entfernt; die Kontextüberwachung bleibt für den Befehl aktiv.

Regression: `TestSSHRunnerBoundsHandshakeAndSession` prüft sowohl einen fehlenden SSH-Banner als auch einen Server, der den Session-Kanal nicht beantwortet, jeweils mit Timeout und explizitem Kontextabbruch. Der lokale SSH-Testserver wird durch den Test selbst erzeugt.

```sh
go test ./internal/provision -run '^TestSSHRunnerBoundsHandshakeAndSession$' -race -count=1
```

## 7. Mittel – Lokale Metrikdaten verschwinden vor dem ersten Archivupload

**Bestätigt. Commit:** `247102c` – `fix: Metrikblöcke vor lokaler Löschung erfolgreich archivieren`.

Das interne Versiegeln führt keine Retention mehr aus; `Maintain` versiegelt, archiviert und bereinigt in dieser Reihenfolge. Bei konfiguriertem Archiv sind noch nicht archivierte Blöcke zusätzlich gegen Alters- und Budgetbereinigung geschützt, auch in den Append-/Seal-Pfaden. Upload-Fehler werden von `Maintain` zurückgegeben. Bei vollem Speicherbudget verweigert Append weitere Samples, statt unarchivierte Daten für neue Samples zu löschen.

Regression: `TestMaintainArchivesExpiredBlocksBeforeDeletion` prüft ein zunächst ausgefallenes und dann wieder erreichbares Archiv, jeweils mit Alters- und zusätzlichem Budgetdruck. Vor dem erfolgreichen Upload müssen die Blöcke lokal erhalten bleiben; danach dürfen sie lokal entfernt werden.

```sh
go test ./internal/metrics -run '^TestMaintainArchivesExpiredBlocksBeforeDeletion$' -race -count=1
```

## 8. Mittel – Host-Restore-Freigabe verlangt globale Einstellungsrechte

**Bestätigt. Commit:** `980d48c` – `fix: Host-Restore mit den Rechten der Backup-Quelle freigeben`.

Host-Operationen speichern ihre `HostSource` und geben sie im API-Operationsmodell aus. Manager und HTTP-Freigabepfad prüfen für einen Host-Restore `restore` auf genau dieser Backup-Quelle im zugehörigen Swarm beziehungsweise global bei externen Quellen. Der OpenAPI-Vertrag enthält das optionale Feld. Alte Operationen ohne Quellfeld behalten die bisherige restriktive Zuordnung; es wird keine Quellidentität geraten.

Regressionen: `TestHostRestoreApprovalUsesSourcePermission` legt mit ausschließlich quellbezogenen Restore-Rechten eine Wiederherstellung an, genehmigt sie und prüft die API-Ausgabe. `TestHostRestoreApprovalResource` prüft die Ressourcenzuordnung für externe und Swarm-Quellen. `TestHostRestoreApprovalRouteUsesSourcePermission` prüft den HTTP-Pfad einschließlich verweigerter Freigabe einer anderen Quelle.

```sh
go test ./internal/manager -run '^TestHostRestoreApprovalUsesSourcePermission$' -race -count=1
go test ./internal/api -run '^TestHostRestoreApproval(Resource|RouteUsesSourcePermission)$' -race -count=1
```

## Nächster Schritt

Der Aufrufer führt die obigen gezielten Regressionstests aus und anschließend die betroffenen Paketsuiten. Laufzeitverhalten, Kompilierbarkeit und Race-Freiheit sind in dieser Sitzung ausdrücklich nicht nachgewiesen. Es wurden keine Release-Gates geschlossen.
