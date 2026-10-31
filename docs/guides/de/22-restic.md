---
title: Restic-Backups einzelner Hosts
---

# Restic-Backups einzelner Hosts [Im Labor geprüft auf arm64 Debian 12 und über den Manager mit dem nativen Agenten; Bereitstellungsanbindung teilweise offen]

Der Manager besitzt ein eigenes Restic-Quellenmodell für Dateisicherungen eines einzelnen verwalteten Hosts. Eine Quelle gehört zu einem Host, nicht zu einem MariaDB-Cluster. Gespeichert werden Quellpfade, ein lokales/SFTP/S3/rest-server-Repository, ein UTC-Cron-Zeitplan, Aufbewahrung und optional ein Prozentsatz für `check --read-data-subset`. Passwort und Backend-Zugangsdaten werden im replizierten Metastore versiegelt und nicht über die API zurückgegeben. Der Runner sendet geschützte Schritte an den eingeschriebenen Host-Agenten; Fehler erzeugen eine Benachrichtigung. Entscheidung und Grenzen: [ADR 0022](../../decisions/0022-native-restic-host-backups.md).

## Bereitstellung ausführbar machen (Opt-in)

In der Ausführungsumgebung des Agenten (normalerweise seinem Container) braucht es ein Restic-Binary, je erlaubter Quell- oder Wiederherstellungswurzel `-restic-allow-path /absoluter/pfad` und benannte Secret-Dateien in seinem `-db-secrets-dir`. `passwordSecret` benennt die Passwortdatei; `credentialSecret` benennt optional eine JSON-Datei mit S3- oder rest-server-Zugangsdaten.

- **Binary.** Das Produkt-Image (`deploy/Dockerfile`) enthält jetzt ein angeheftetes Restic (Build-Argumente `RESTIC_VERSION`, `RESTIC_SHA256_AMD64/ARM64`), das vom Upstream-Release geladen und mit `sha256sum -c` geprüft wird; bei Abweichung bricht der Build ab. `scripts/check_deploy.py` prüft die Anheftung. Das Image hat keinen `ssh`-Client, **SFTP-Repositories brauchen daher ein anderes Agent-Image** (oder ein anderes Backend). Restic ist statisch und läuft im shell-losen Distroless-Image.
- **Allowlist-Flags, Mounts und Secrets.** Restic ist aus, solange die Agent-Bereitstellung es nicht ausdrücklich anfordert. `swarm.AgentMTLS.Restic` (`AgentRestic{AllowPaths, Secrets}`) ergänzt je Wurzel ein `-restic-allow-path` **und einen Bind-Mount am selben Pfad**, bindet die benannten, bereits vorhandenen Swarm-Secrets unter `/run/secrets` ein und setzt `RESTIC_CACHE_DIR` im Agent-Zustandsverzeichnis. Wurzeln müssen saubere absolute Pfade unterhalb von `/` sein; Docker-Socket, `/run/secrets` und das Agent-Zustandsverzeichnis sowie eine leere Liste werden abgelehnt. Ohne die Option bleibt die Dienstspezifikation unverändert. Die Mounts sind schreibbar, weil Wiederherstellungen in neue Pfade dort schreiben.
- **Nicht automatisiert.** Der Manager erzeugt die Swarm-Secrets nicht aus den versiegelten Quellen-Zugangsdaten, aktualisiert keinen bereits angelegten globalen Agent-Dienst (der Aufruf legt ihn nur an; geänderte Wurzeln brauchen ein Service-Update oder Neuanlage) und initialisiert keine Repositories und keine SFTP-Schlüssel. Die Betreiberin führt `restic init` einmal je Repository aus. `AgentRestic` ist weder in der Manager-Konfigurationsdatei noch in einer UI verdrahtet (nur Bibliotheksebene, Unit-Tests).
- **Begrenzter Start.** Jedem Restic-Befehl geht ein begrenzter Test `restic cat config` voraus (2 Minuten). Echte Läufe zeigten, dass falsche S3-Zugangsdaten Restic länger als zehn Minuten wiederholen ließen; der Schritt endet jetzt mit der klaren Meldung „did not answer in time“. Ein abgebrochener oder beendeter Befehl wird als unterbrochen gemeldet und weist auf ein mögliches veraltetes Lock hin; der Wrapper führt nie `unlock` aus.

Der [Laborlauf E17](../../../tests/p0/results/e17-restic.md) ging den Produktweg (Modulschalter, Host-Quellen-API, nativer Agent als root mit der Unit-Härtung, S3 auf MinIO über TLS): Backup, Aufbewahrung, Prüfung, freigegebene Wiederherstellung in einen neuen Pfad und Verifikation bestanden nach zwei Korrekturen; S3-Orte müssen https verwenden (`s3:http://` wird abgelehnt). Der [Laborlauf E11](../../../tests/p0/results/e11-restic.md) hat die echten Restic-Wrapper gegen lokale, SFTP-, rest-server- (TLS) und S3-Repositories (MinIO, TLS) sowie einen externen Host über SSH ausgeführt. Was genau geprüft wurde und was nicht, steht dort unter „Verified“ und „Not verified“.

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

Quellen mit `kind: "external-host"` haben keine Swarm-Zuordnung. Für `transport: "ssh-plan"` sind `sshAddress`, ein nicht privilegierter `sshUser` sowie beim Anlegen die nur schreibbaren Felder `sshPrivateKey` und ein festgehaltener `sshKnownHosts`-Eintrag erforderlich. Der Manager versiegelt diese Werte und überträgt Restic-Passwort und Backend-Zugangsdaten über SSH-stdin. Das entfernte Skript legt eine private temporäre Passwortdatei an, startet Restic und entfernt die Datei; die Zugangsdaten stehen nicht im SSH-Kommando. Der SSH-Pfad hat ein 64-KiB-Ausgabelimit und auf der Gegenseite keine Allowlist: Quellen und Wiederherstellungsziele werden nur durch die Dateirechte des entfernten Kontos begrenzt (dediziertes nicht privilegiertes Konto verwenden). Der festgehaltene `known_hosts`-Eintrag muss den Host-Key-Typ enthalten, den der Server tatsächlich anbietet; der Runner bietet jetzt nur die festgehaltenen Typen an. Der Pfad lief einmal gegen einen echten OpenSSH-Server (Labor E11). `transport: "agent"` liefert bis zur eigenständigen Einschreibung externer Hosts 503. Eine UI gibt es noch nicht. Für Clusterdaten gilt weiterhin die [Datenbank-Backup-Anleitung](05-backup-restore.md).

## Agent-Laufzeit (ADR 0024)

Host-Quellen zeigen `runtime` (`native` oder `container`) und `runtimeNote`: Restic läuft auf dem nativen Agenten, wenn er die Fähigkeit `restic` meldet, sonst auf dem Container-Agenten, nie stillschweigend anderswo. Meldet der Host keines von beiden, antwortet der Start eines Backups mit 409 `agent_runtime_unavailable`. Siehe [Anleitung 24](24-agent-runtimes.md).
