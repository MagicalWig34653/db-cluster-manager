---
title: Restic-Backups einzelner Hosts
---

# Restic-Backups einzelner Hosts [Fakes]

Der Manager besitzt ein eigenes Restic-Quellenmodell für Dateisicherungen eines einzelnen verwalteten Hosts. Eine Quelle gehört zu einem Host, nicht zu einem MariaDB-Cluster. Gespeichert werden Quellpfade, ein lokales/SFTP/S3/rest-server-Repository, ein UTC-Cron-Zeitplan, Aufbewahrung und optional ein Prozentsatz für `check --read-data-subset`. Passwort und Backend-Zugangsdaten werden im replizierten Metastore versiegelt und nicht über die API zurückgegeben. Der Runner sendet geschützte Schritte an den eingeschriebenen Host-Agenten; Fehler erzeugen eine Benachrichtigung. Entscheidung und Grenzen: [ADR 0022](../../decisions/0022-native-restic-host-backups.md).

In der Ausführungsumgebung des Agenten (normalerweise seinem Container) braucht es ein Restic-Binary, je erlaubter Quell- oder Wiederherstellungswurzel `-restic-allow-path /absoluter/pfad` und benannte Secret-Dateien in seinem `-db-secrets-dir`. `passwordSecret` benennt die Passwortdatei; `credentialSecret` benennt optional eine JSON-Datei mit S3- oder rest-server-Zugangsdaten. Das derzeitige Produkt-Agent-Image enthält Restic nicht, und die Manager-Bereitstellung ergänzt einen vorhandenen globalen Agent-Dienst **nicht** um diese Mounts, Allowlist-Flags oder Secrets. Das Anlegen einer API-Quelle macht eine bestehende Bereitstellung deshalb noch nicht ausführbar. Repository-Initialisierung und SFTP-Schlüssel/Host-Key-Bereitstellung sind ebenfalls nicht automatisiert.

## API und Rechte

`GET /api/v1/host-sources` und `GET /api/v1/host-sources/{id}` zeigen für die Sitzung sichtbare Metadaten. Administratoren dürfen Quellen per `POST /api/v1/host-sources`, `PUT /api/v1/host-sources/{id}` und `DELETE /api/v1/host-sources/{id}` verwalten. `password` und `credentials` sind nur beim Schreiben enthalten; Secret-Namen und Werte sind getrennt. Operatoren mit passendem Bereich dürfen per `POST /api/v1/host-sources/{id}/backups` eine Runner-Operation starten. Viewer dürfen nur lesen. Zustandsändernde Berechtigungsentscheidungen werden auditiert.

Beispiel für einen Request-Body, ausschließlich mit erfundenen Zugangsdaten:

```json
{
  "id": "host-home",
  "name": "Home-Verzeichnis",
  "kind": "managed-host",
  "transport": "agent",
  "swarm": "sw1",
  "hostId": "node-1",
  "paths": ["/srv/home"],
  "excludes": ["/srv/home/cache"],
  "backend": "s3",
  "repository": "s3:backup.example.test/host-home",
  "passwordSecret": "restic-home-password",
  "credentialSecret": "restic-home-s3",
  "password": "nur-erfundenes-beispiel",
  "credentials": {"AWS_ACCESS_KEY_ID": "synthetic", "AWS_SECRET_ACCESS_KEY": "synthetic"},
  "schedule": "0 2 * * *",
  "retention": {"keepDaily": 7, "prune": true},
  "checkSubset": "5%"
}
```

`POST /api/v1/host-sources/{id}/restore` erhält `{"snapshot":"<restic-id>","target":"/srv/neuer-pfad"}`. Die Operation wartet auf eine separate Freigabe ihres Digests über den vorhandenen Freigabe-Endpunkt. Der Agent verweigert vorhandene Ziele und Pfade außerhalb seiner Allowlist. Er legt das neue Ziel atomar an und verwendet `--overwrite never`; Live-Daten dürfen nicht als Ziel dienen. Eine erfolgreiche Teilprüfung belegt die Repository-Integrität nur für den geprüften Anteil und ersetzt keinen Test-Restore.

Quellen mit `kind: "external-host"` haben keine Swarm-Zuordnung. Für `transport: "ssh-plan"` sind `sshAddress`, ein nicht privilegierter `sshUser` sowie beim Anlegen die nur schreibbaren Felder `sshPrivateKey` und ein festgehaltener `sshKnownHosts`-Eintrag erforderlich. Der Manager versiegelt diese Werte und überträgt Restic-Passwort und Backend-Zugangsdaten über SSH-stdin. Das entfernte Skript legt eine private temporäre Passwortdatei an, startet Restic und entfernt die Datei; die Zugangsdaten stehen nicht im SSH-Kommando. Dieser SSH-Pfad wurde nur mit Fakes geprüft und hat ein 64-KiB-Ausgabelimit. `transport: "agent"` liefert bis zur eigenständigen Einschreibung externer Hosts 503. Eine UI gibt es noch nicht. Kein Befehl wurde mit echtem Restic, einem echten Ziel oder einem externen Host ausgeführt. Für Clusterdaten gilt weiterhin die [Datenbank-Backup-Anleitung](05-backup-restore.md).
