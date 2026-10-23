---
title: Betriebshandbuch
---

# Betriebshandbuch

[Übersicht](index.md) | Zurück: [Bekannte Grenzen](10-known-limits.md)

Abläufe für die Person, die den Manager betreibt. Jeder Schritt nennt den Prüfstand. Wo es keine Produktfunktion gibt, ist der manuelle Ausweg mit [Ungetestet] gekennzeichnet: Er folgt aus der Art, wie der Zustand gespeichert wird, aber niemand hat daraus eine Wiederherstellung durchgeführt.

## 1. Manager-Zustand sichern

**Produktfunktion:** `internal/backup` kann verschlüsselte Sicherungen der Manager-Konfiguration schreiben, aber kein Planer, keine API und keine Oberfläche ruft es auf [Fakes]. Das R1-Tor „Neuinstallation und Konfigurationswiederherstellung dokumentiert und reproduziert“ ist damit offen.

**Manueller Ausweg [Ungetestet]** (verwalteter Modus, Einzelinstanz):

1. Pfade aus Ihrer Konfiguration notieren: `manager.dataDir` (Metastore, `data.key`, Monitoring-Speicher) und die Konfigurationsdatei.
2. Manager für eine konsistente Kopie stoppen (der Zustand ist ein Raft-Log mit Snapshots in bbolt; keine laufende Instanz kopieren).
3. `dataDir` und Konfigurationsdatei auf Speicher außerhalb des Hosts kopieren. **`data.key` entschlüsselt Konten, Sitzungen, Passkeys und Benachrichtigungs-Secrets: Kopie verschlüsselt und getrennt von den Datenbank-Backups aufbewahren.**
4. Manager starten und `GET /api/v1/health` prüfen.
5. Außerdem aufbewahren: das Recovery-Key-Secret der Cluster-Backups, die Ziel-Konfigurationsdatei (S3/SFTP-Zugangsdaten), Ihre Wiederherstellungscodes sowie die Liste der von Ihnen angelegten Swarm-Secrets und Netzwerke (Namen, nicht Werte).

Testen Sie die Wiederherstellung auf einem Übungsrechner, bevor Sie sie brauchen: `dataDir` mit derselben `nodeId` zurückkopieren, Manager starten, mit Passkey anmelden.

## 2. Manager aktualisieren

Seit es Zustand gibt, wurde kein Upgrade durchgeführt, und eine Schema-Migrationsrichtlinie ist nicht dokumentiert [Ungetestet].

1. Release-Hinweise und [Bekannte Grenzen](10-known-limits.md) lesen. Zuerst die Sicherung aus Abschnitt 1 anlegen.
2. Kompatibilitätsmatrix gegen Ihre Docker-Version prüfen.
3. Einzelinstanz, Container: `DBCM_IMAGE=<neu> docker compose -f deploy/compose.yaml up -d` (Volume bleibt). Prozess-Installation: Binärdatei ersetzen, Unit neu starten. `GET /api/v1/version` und `/api/v1/health` prüfen.
4. Die Agenten laufen mit dem in den Manager-Einstellungen genannten Image. Nach einem Wechsel des Agent-Images `POST /api/v1/swarms/{id}/agents/deploy` ausführen und prüfen, dass jeder Host `agentIdentity: true` meldet [Ungetestet].
5. Laufende Operationen überstehen einen Neustart nur, soweit der Runner sie fortsetzt (`autoResume`); `GET /api/v1/operations` prüfen, anstehende fortsetzen oder abbrechen. Nicht während Backup, Wiederherstellung oder Recovery aktualisieren.
6. Zurückrollen: vorheriges Image und Zustandskopie aus Schritt 1 wiederherstellen. Große Versionssprünge brauchen künftig ausdrückliche Freigabe.

Datenbank-(MariaDB-)Upgrades sind nicht implementiert; große Upgrades brauchen immer Ihre ausdrückliche Freigabe.

## 3. Labor-Bereinigung (nur Testumgebungen)

Labor-VMs werden nur mit `bin/lab` angelegt und entfernt. Es führt ein Besitzmanifest (`.lab/manifest.json`) und fasst nur Namen mit Präfix `dbcm-lab-` an.

```sh
make build
bin/lab status             # nur lesend: Host-Kapazität, Budget, Manifest, fremde Lima/Colima-Einträge
bin/lab cleanup            # Vorschau des auf das Manifest begrenzten Abbaus
bin/lab cleanup -apply     # führt ihn aus; bricht ab, wenn ein fremder Eintrag verschwand
```

Die Experiment-Skripte unter `tests/p0/` legen ihre VMs selbst an und entfernen sie per `bin/lab cleanup -apply` in einer Exit-Falle [Lab-VMs]. Regeln: Laborbudget einhalten (16 GB RAM, 50 % CPU, etwa 100 GB Platte), nie `docker system prune`, nie fremde Colima- oder Lima-Profile entfernen, keine Produktions-Zugangsdaten und -Backups ins Labor. Der freigegebene Speicherplatz nach der Bereinigung wurde nicht geprüft (in W04 war der freie Platz danach geringer). Host-seitige Build-Artefakte im ignorierten Verzeichnis `.lab/` können Sie von Hand löschen.

## 4. Manager-Installation entfernen

`docker compose -f deploy/compose.yaml down` (mit `-v` wird das Volume `manager-data` gelöscht) oder `docker stack rm dbcm` samt Volume auf jedem Knoten. Der Manager entfernt keine von ihm angelegten Agenten, Datenbankdienste oder Volumes. Löschen Sie Cluster zuerst über die API (Freigabe nötig, siehe [Cluster anlegen](03-create-cluster.md)); entfernen Sie dann den Agent-Dienst, die Host-Verzeichnisse unter der Volume-Basis sowie Netzwerke und Secrets des Betreibers namentlich. Nie global aufräumen.
