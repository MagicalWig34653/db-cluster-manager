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

## 4. Host-Wartung und Drift

**Host-Wartung** [Fakes]: `GET /api/v1/hosts/{id}/maintenance-plan` zeigt je Cluster mit einem Knoten auf dem Host vorab die Schritte, Warnungen und warum ein Cluster ablehnt (weniger als eine strikte Mehrheit seiner Knoten außerhalb des Hosts, eine unfertige Operation). `POST /api/v1/hosts/{id}/maintenance` (optional `reason`) stoppt diese Knoten geordnet nacheinander, jeden erst, wenn alle Knoten des Clusters auf anderen Hosts Synced sind; der Ersatz auf diesem Host ist pausiert. Lehnt ein Cluster ab, antwortet es mit 409 und ändert nichts. `Host.maintenance.ready` sagt, wann Sie den Host selbst neu starten dürfen: **Das Produkt startet nie einen Host neu, leert ihn nicht und ändert seine Swarm-Verfügbarkeit nicht.** `DELETE /api/v1/hosts/{id}/maintenance` prüft jeden Identitätsmarker der Volumes, startet die Knoten nacheinander und wartet auf Synced (IST oder SST). Braucht die Berechtigung Betreiben auf dem Host und seinen Clustern; auditiert.

**Drift** [Fakes]: `GET /api/v1/clusters/{id}/drift` vergleicht den Cluster-Datensatz mit den Swarm-Diensten mit seinem Label (fehlend, außerhalb des Managers neu angelegt, fremde Labels, geänderte Platzierung, auf 0 skaliert oder ohne laufenden Task außerhalb von Wartung und Operationen, unerwartete Dienste). Jeder Befund schlägt einen manuellen Schritt vor; nichts wird geändert. Der Leader prüft auch periodisch und erfasst ein Ereignis, wenn sich die Befunde ändern.

## 5. Replikate, Schlüsselring und Peer-Zertifikate

Nichts davon lief mit mehreren Manager-Replikaten auf VMs [Ungetestet]; die Teile sind mit Fakes und Loopback-TLS getestet [Fakes]. Referenz: [Konfigurationsreferenz](12-configuration-reference.md), [ADR 0013](../../decisions/0013-replica-keys-peer-tls-and-revocation.md).

1. **Peer-Zertifikate:** `DBCM_PEER_CA_PASSPHRASE=<mindestens 12 Zeichen> manager -issue-peer-certs ./peers -installation inst-1 -peer-ids manager-1,manager-2,manager-3`. Das schreibt `ca.pem`, `ca.sealed` (die verschlüsselte CA) und je Replikat ein Verzeichnis mit `peer.pem` und `peer-key.pem`. Jedes Replikat-Verzeichnis auf seinen Host kopieren; `ca.sealed` und die Passphrase bleiben offline. Der Befehl überschreibt keine vorhandene CA.
2. **Konfiguration:** auf jedem Replikat `peers` (dieselbe Liste), `bootstrap: true` für den ersten Start, `keyring: true` und `peerTLS` setzen. Peer-Zertifikate erneuert das Produkt nicht: vor Ablauf (Standardgültigkeit 365 Tage) mit der versiegelten CA neu ausstellen; das Replikat liest eine geänderte Zertifikatsdatei ohne Neustart neu.
3. **Erstes Replikat:** starten. Eine vorhandene `data.key` wird als erster Schlüssel des Schlüsselrings übernommen; es wird keine neue Schlüsseldatei angelegt.
4. **Weitere Replikate registrieren:** auf jedem `manager -print-replica-key -config <Datei>` ausführen (legt bei Bedarf `<dataDir>/replica.key` an) und den öffentlichen Schlüssel samt Fingerabdruck notieren. Auf einem anderen Weg vergleichen, dann als Administrator `PUT /api/v1/settings/replicas/{id}/data-key-recipient` mit dem öffentlichen Schlüssel (`dbcmpk1:...`) aufrufen. Nur aktuelle Voter werden akzeptiert; nichts wird automatisch registriert. Ein Replikat, das ohne den Schlüssel Leader wird, gibt die Leadership ab, ein nicht registriertes Replikat bedient die API also nicht. `GET /api/v1/settings/replicas` zeigt den Zustand des Schlüsselrings und ob die Schlüssel `recoverable` sind.
5. **Wiederherstellung:** Ist `settings.backup.recipientKey` konfiguriert, hat jeder Datenschlüssel auch einen Umschlag für den Backup-Recovery-Key; bewahren Sie dessen privaten Teil außerhalb des Clusters auf. `<dataDir>` wie in Abschnitt 1 sichern.
6. **Replikat abschneiden:** `POST /api/v1/settings/replicas/{id}/revoke-certificates` sperrt seine Manager-Zertifikate an jedem Agenten. Schlüssel, die das Replikat schon entpackt hat, nimmt das nicht zurück; Rotation ist nicht implementiert.

## 6. Manager-Installation entfernen

`docker compose -f deploy/compose.yaml down` (mit `-v` wird das Volume `manager-data` gelöscht) oder `docker stack rm dbcm` samt Volume auf jedem Knoten. Der Manager entfernt keine von ihm angelegten Agenten, Datenbankdienste oder Volumes. Löschen Sie Cluster zuerst über die API (Freigabe nötig, siehe [Cluster anlegen](03-create-cluster.md)); entfernen Sie dann den Agent-Dienst, die Host-Verzeichnisse unter der Volume-Basis sowie Netzwerke und Secrets des Betreibers namentlich. Nie global aufräumen.
