---
title: Notfallwiederherstellung und Backup-Kopien
---

# Notfallwiederherstellung und Backup-Kopien

[Handbuchübersicht](index.md) | Zurück: [Provisionierung](19-provisioning.md)

Alles in diesem Handbuch ist nur mit Fakes geprüft [Fakes]: In-Memory-Speicher, ein `httptest`-S3-Server, ein SFTP-Server im Prozess und ein In-Memory-Raft-Replikat. Nichts lief auf einem echten Swarm, S3-Dienst oder SFTP-Server. Entwurf: [ADR 0021](../../decisions/0021-backup-targets-copies-and-metastore-backup.md).

## Was Sie außerhalb des Managers aufbewahren müssen

| Objekt | Warum |
| --- | --- |
| Der **Backup-Wiederherstellungsschlüssel** (private Hälfte; der Manager kennt nur die öffentliche Hälfte in `settings.backup.recipientKey`) | Öffnet Cluster-Backups, Metastore-Backups und die Wiederherstellungs-Umschläge der Datenschlüssel. Ohne ihn sind alle unlesbar. |
| Die **Backend-Konfigurationsdateien** (S3/SFTP-Zugangsdaten, optionaler `encryption`-Schlüssel) | Erreichen die Ziele und entschlüsseln clientseitig verschlüsselte Objekte. |
| Die Manager-Konfiguration (`manager.json`) und die Liste der von Ihnen angelegten Swarm-Secrets und Netze | Namen, keine Werte. |

## Backup-Ziele

`manager.backup.backendConfigFile` ist das Hauptziel. `manager.backup.targets` fügt benannte Ziele hinzu (Name zu Backend-Konfigurationsdatei) für Kopien und Metastore-Backups. Eine Backend-Konfiguration hat den Typ `s3`, `sftp`, `local` (Alias `dir`) und optional clientseitige Verschlüsselung:

```json
{"type": "sftp", "sftp": {"addr": "backup.example:22", "user": "dbcm", "dir": "/srv/dbcm",
  "host_key_fingerprint": "SHA256:..."},
 "encryption": {"key_id": "t1", "key": "<base64 von 32 Zufallsbytes>", "previous": {"t0": "<base64>"}}}
```

Mit `encryption` wird jedes Objekt (Manifeste, Statusdateien, Transfer- und Metastore-Records) verschlüsselt, bevor es den Prozess verlässt; das Ziel sieht nur Chiffrat und Namen. Nach einem Schlüsselwechsel behalten Sie den alten Schlüssel unter `previous`, damit ältere Objekte lesbar bleiben. Cluster-Datenströme sind immer für den Wiederherstellungsschlüssel verschlüsselt. Ein `local`-Ziel taugt nicht für Backup-Jobs, die aus ihrem eigenen Container schreiben (siehe [Backups und Wiederherstellung](05-backup-restore.md)).

## Ein Backup in einen anderen Cluster, Swarm oder Standort kopieren

1. Tragen Sie Ihre Ziele in `manager.backup.targets` ein und starten Sie den Manager neu.
2. `POST /api/v1/clusters/{id}/backup-copies` mit `{"backup": "<id>", "target": "<name>"}` (Berechtigung: backup auf dem Cluster, auditiert). Antwort 202; fragen Sie `GET /api/v1/backup-copies` ab.
3. Der Leader kopiert den Stream, prüft ihn beim Kopieren gegen den Manifest-Digest (bei Abweichung wird die Kopie gelöscht und der Lauf schlägt fehl), schreibt dann das Manifest und zuletzt einen HMAC-signierten Transfer-Record. Eine fehlgeschlagene oder unterbrochene Kopie erzeugt ein Ereignis und eine Benachrichtigung (`backup.copy_failed`) und kann einfach neu gestartet werden.
4. Die Aufbewahrung des Quellsatzes löscht dessen Kopien auf den Zielen (Transfer-Record zuerst). Der neueste und der neueste geprüfte Satz behalten ihre Kopien.

Eine Kopie ändert nichts: keinen Cluster, keine Route, keinen Proxy.

## Wiederherstellung aus einer Kopie: ausdrückliche Freigabe, nie automatisch

1. `POST /api/v1/backup-copies/{id}/restore-proposal` mit `{"targetSwarm": "...", "targetCluster": "neuer-name"}` (Berechtigung: restore, auditiert). Der Manager prüft Signatur, Manifest und Stream-Digest und liefert einen **Vorschlag** mit Digest. `approvalRequired` ist immer `true`, `autoPromotion` immer `false`. Der Name darf keinem bestehenden Cluster gehören.
2. Eine Person prüft ihn. Die Freigabe bindet an den genauen Digest; der Wiederherstellungsplan erzeugt immer einen **neuen** Cluster mit neuer Identität, verweigert Identität, Namen, Netze und Volumes des Quellclusters, und jeder zerstörerische Schritt wartet auf die Freigabe ([Backups und Wiederherstellung](05-backup-restore.md)).
3. Clients auf den wiederhergestellten Cluster umzustellen ist eine getrennte, manuelle Entscheidung. Der wiederhergestellte Cluster beginnt eine neue Galera-Historie.

[Fakes]: Vorschlag, Freigabe und Plan existieren in Code und Tests; der HTTP-Endpunkt liefert nur den Vorschlag. Die Ausführung der Wiederherstellung einer Kopie über die API ist noch nicht angeschlossen. Die Kopier-Routen (`/backup-copies`, `/clusters/{id}/backup-copies`, `/backup-copies/{id}/restore-proposal`) und die Metastore-Backup-Routen sind in der Managed-Laufzeit eingehängt, sobald `manager.backup` konfiguriert ist (ein Test startet die echte Laufzeit mit einem `targets`-Eintrag und erreicht sie; das Anlegen eines Metastore-Backups antwortet weiterhin mit 409 `settings_incomplete`, bis `settings.backup.recipientKey` gesetzt ist).

## Geplante Wiederherstellungsprüfung

Ergänzen Sie `verify` in der Backup-Richtlinie eines Clusters (`PUT /api/v1/clusters/{id}/backup-policy`):

```json
{"enabled": true, "schedule": "0 2 * * *", "verify": {"enabled": true, "schedule": "0 4 * * 0", "maxMinutes": 90}}
```

Nach diesem Zeitplan stellt der Leader das neueste abgeschlossene physische Backup in einer isolierten Inspektionsinstanz wieder her (derselbe Plan wie eine manuelle Inspektion: wsrep aus, eigenes Netz, Produktionskonten gesperrt), lässt das Prüf-Gate sie abfragen und baut sie wieder ab. Das Einschalten braucht die Berechtigung restore und gilt als Freigabe dieser isolierten Inspektionen. Eine fehlgeschlagene Prüfung, eine fehlgeschlagene oder abgebrochene Operation, ein Lauf über `maxMinutes` oder einer, der nicht starten kann, erzeugt ein Ereignis und eine Benachrichtigung (`backup.restore_verification_failed`); ein Erfolg sendet `backup.restore_verification_passed` (Info). Grenzen: Der Bericht ist auf Instanzebene, Tabelleninhalte werden nicht verglichen, und ein Inspektionshost muss angegeben werden (`verify.host`), weil freier Plattenplatz nicht beobachtet wird.

## Das eigene Metastore des Managers sichern

`POST /api/v1/settings/metastore-backups` (Berechtigung: backup auf den Einstellungen) oder per `manager.backup.metastoreBackupHours` nach Zeitplan. Es braucht `settings.backup.recipientKey` und ein Ziel (`metastoreTarget`, Standard das Hauptbackend). Der Export enthält Cluster, Hosts, Swarms, Backup-Richtlinien und -Einträge, Einstellungen, Konten, Rollen, Passkeys, die Agent-CA, den Schlüsselring und versiegelte Secrets. Ausgelassen werden Sitzungen, Operationen, Freigaben und Ereignisse, damit nach einer Wiederherstellung nichts von selbst weiterläuft. Er ist für den Wiederherstellungsschlüssel verschlüsselt; der laufende Manager kann ihn nicht wieder lesen. `GET /api/v1/settings/metastore-backups` listet Backups, deren signierter Record stimmt; `metastoreBackupKeep` (Standard 14) begrenzt, wie viele bleiben. Ein fehlschlagendes geplantes Backup erzeugt ein Ereignis und eine Benachrichtigung (`backup.metastore_failed`).

## Betriebsanleitung: der Manager ist verloren

1. **Entscheiden, ob nötig.** Fällt nur ein Replikat von mehreren aus, stellen Sie nicht wieder her: ersetzen Sie es über die Mitgliedschaft. Wiederherstellen nur, wenn kein Replikat mit den Daten mehr existiert. [Ungetestet]
2. **Host vorbereiten**, der den Manager betreiben kann (Swarm-Manager-Knoten mit Docker-Socket, gleiches Image). Konfigurationsdatei ablegen. Ein altes Datenverzeichnis zur Seite legen; der Import verweigert ein nicht leeres `metastore`-Verzeichnis.
3. **Holen** Sie das neueste `metastore-<Installation>-<Zeit>.dbcms` und seine `.record.json` vom Ziel. Legen Sie den Wiederherstellungsschlüssel in eine nur für Sie lesbare Datei.
4. **Importieren** Sie bei gestopptem Manager, als der Benutzer, der ihn betreibt:

   ```
   manager metastore-import -config /etc/dbcm/manager.json \
     -in metastore-inst-20261026t020000.dbcms -recovery-key-file /sicher/recovery.key
   ```

   Es öffnet den Export, startet ein Ein-Stimmen-Metastore auf `manager.bind`, schreibt die Dokumente (Schlüsselring zuerst, Datenschlüssel für die Identität `replica.key` dieses Replikats neu verpackt, bei Fehlen angelegt), gibt aus, was es tat, und stoppt. Löschen Sie danach die Schlüsseldatei.
5. **Manager starten.** Mit Passkey oder Wiederherstellungscode anmelden (Sitzungen wurden nicht wiederhergestellt). Das einzelne Replikat ist durch den Import registriert. Weitere Replikate kommen über das normale Mitgliedschaftsverfahren hinzu.
6. **Abgleichen, bevor Sie handeln.** Der wiederhergestellte Zustand ist so alt wie das Backup. Vergleichen Sie Hosts, Dienste und Cluster mit dem laufenden Swarm (`Sync Swarm`, Drift-Berichte) und prüfen Sie, ob die Zertifikate der Agents noch funktionieren. Zum Exportzeitpunkt laufende Operationen sind weg; prüfen Sie die Gesundheit jedes Clusters, bevor Sie etwas starten. Hosts, deren Zertifikate sich nach dem Backup geändert haben, neu registrieren.
7. **Backups prüfen.** Für einen Cluster ein Backup, eine Inspektionswiederherstellung und ein Metastore-Backup ausführen, um die Kette erneut zu beweisen.

Nicht abgedeckt: ein verlorener Wiederherstellungsschlüssel (dann sind weder Cluster- noch Metastore-Backups lesbar), ein verlorener Schlüsselring ohne Wiederherstellungs-Umschlag (Secrets bleiben unlesbar), eine Wiederherstellung in einen Manager einer anderen Installations-ID und ein verlorener Swarm (zuerst den Swarm neu aufbauen; Agents und Secrets stellt dieses Verfahren nicht wieder her).

## Prüfstufen

| Teil | Stufe |
| --- | --- |
| Signierte Dokumente, clientseitig verschlüsseltes Backend über lokale, S3- (`httptest`) und SFTP-Ziele (im Prozess), Tests auf Manipulation, Abschneiden und Vertauschen | [Fakes] |
| Kopie zwischen Backends, Prüfung, Kopplung an die Aufbewahrung, Wiederherstellungsvorschlag mit Freigabe-Gate, API-, OpenAPI- und Berechtigungstests | [Fakes] |
| Geplante Wiederherstellungsprüfung mit Fehlerbenachrichtigung | [Fakes] (Fake-Agents; keine echte Inspektionsinstanz) |
| Metastore-Export/-Import, Offline-Befehl `metastore-import`, geplantes Metastore-Backup | [Fakes] (ein In-Memory-Raft-Replikat); die Anleitung lief nie auf einem echten Manager |
| Ausführung der Wiederherstellung einer Kopie über die API; installationsübergreifende Prüfung; UI | [Ungetestet] |
