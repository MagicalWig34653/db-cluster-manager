---
title: Bekannte Grenzen
---

# Bekannte Grenzen

[Übersicht](index.md) | Zurück: [Fehlersuche](09-troubleshooting.md) | Weiter: [Betriebshandbuch](11-operator-runbook.md)

Entnommen aus [STATUS.md](../../STATUS.md) vom 2026-10-23. Die R1-Tore in [RELEASES.md](../../RELEASES.md) sind **nicht** erfüllt, ebenso nicht die P0-Ausgangstore. Dies ist keine Produktionssoftware, und es gab keinen Swarm-Test.

## Nachweisbasis

Nachweise mit echten Komponenten gibt es nur von Lima-VMs auf einem Apple-Silicon-Host (arm64, Docker 29.9.2, MariaDB 11.4.13): E1-E6. Die meisten Funktionen wurden einmal geprüft. Alles andere wurde nur mit Fakes getestet. Zeiten aus E4 wurden unter Fremdlast des Hosts gemessen.

## Nicht nachgewiesen oder fehlend

- **Plattformen:** Für amd64 gibt es keinen Nachweis. Nur Docker 29.x steht in der Kompatibilitätsmatrix. Kubernetes liegt außerhalb von R1.
- **Manager-HA:** Drei Manager-Replikate liefen als normale Prozesse auf Labor-VMs (E4 B2), auch mit Leader-Abschüssen; für den Drei-Dienste-Swarm-Stack, die Sperrlistenverteilung und die neuen HA-Funktionen gibt es nur statische/Fake-Nachweise. Schlüsselrotation/Neuverschlüsselung und Mitgliedschaftsänderungen haben nur Fake-Nachweise; Mitgliedschafts-Endpunkte antworten im Standardbetrieb mit 503. Die Erneuerung der Peer-Zertifikate, ablaufbezogene Bereinigung der Sperrliste und mTLS-Weiterleitung sind nur mit Fakes geprüft. Echte Binlog-Wiedergabe fehlt. Jedes Replikat braucht einen lokalen Docker-Socket eines Swarm-Managers. `deploy/stack.yaml` bleibt eine Einzelinstanz; `deploy/stack-ha.yaml` ist ein statisch geprüfter, noch nie ausgeführter Drei-Dienste-Entwurf.
- **Paketierung:** Manager-Image und Einzelinstanz-Stack liefen auf arm64-Labor-VMs (E7); Job-Image, Drei-Dienste-HA-Stack, Compose und Release-Workflow bleiben ungeprüft.
- **Datenbankzugriff:** Der Manager stellt keine Proxys bereit. E8 prüfte manuell redundante Proxys und eine VIP auf VMs; die bisherige lokale Wahl ergab unterschiedliche Schreibknoten. Die gemeinsame Schreibknoten-Wahl ist jetzt nur mit Fakes geprüft und braucht einen E8-Neulauf. Ein nicht erreichbarer registrierter Proxy blockiert den Wechsel. Kein Knoten-TLS oder Proxy-zu-Knoten-TLS; Client-TLS gibt es nur im Proxy, ohne Zertifikatsausstellung.
- **Betrieb:** Host-Wartung, Drift-Erkennung und Cluster-Löschung existieren [Fakes]; das Produkt startet nie einen Host neu und leert ihn nie, Drift schlägt nur manuelle Schritte vor. Freigabepflichtiges Hinzufügen/Entfernen von Knoten, rollende Neustarts und Patch-Upgrades derselben Serie sind über die Cluster-API vorhanden [Fakes]; siehe [Betriebshandbuch](11-operator-runbook.md#7-galera-skalieren-und-rollend-warten). Kein Ersatzhost-Ablauf, kein Upgradepfad über Seriengrenzen, keine Migration, kein phpMyAdmin, kein PITR. Beitretende Knoten nutzen immer SST.
- **Backups:** keine geprüfte Vollwiederherstellung produktiver Daten; Wiederherstellung in einen neuen Cluster, logisches Backup, Aufbewahrung und der Zeitplaner liefen nicht auf VMs; das Backup des Manager-Metastores, Backup-Kopien zwischen Zielen, geplante Wiederherstellungsprüfung und clientseitig verschlüsselte Ziele gibt es nur mit Fakes ([Handbuch 21](21-disaster-recovery.md)); der Metastore-Import lief nie auf einem echten Manager, die Wiederherstellung aus einer Kopie über die API ist nicht angeschlossen, Transfer-Records lassen sich nur mit dem Schlüsselring der schreibenden Installation prüfen, und die geplante Prüfung braucht einen ausdrücklich genannten Inspektionshost. Manifeste im Format 2 nutzen Digests mit Schlüssel, ältere Klartext-Digests. Die Wiederherstellungsprüfung vergleicht keine Tabelleninhalte.
- **Monitoring:** Ereignis und Benachrichtigung bei Totalausfall gibt es nur mit Fakes; für viele Standard-Alarmreihen fehlt ein echter Sammler; Benachrichtigungskanäle gegen echte Empfänger ungetestet.
- **Autorisierung:** Die Freigabe der Totalausfall-Wiederherstellung braucht die Berechtigung `recover` (nur Administrator-Preset) [Fakes]. Echte Hardware-Authenticator, Safari/Firefox und Screenreader ungetestet.
- **Löschen:** Das Löschen eines Clusters lässt Host-Verzeichnisse lokaler Volumes und Backups im Ziel bewusst bestehen und lief nur gegen Fakes.
- **Belastbarkeit:** Ressourcenerschöpfung und Kapazitätsverweigerung, Neustart während der Bereinigung, VM-Stromausfall und geordnetes Leeren eines Hosts wurden nicht getestet.
- **Laborrisiko:** Lima legt Gast-Listener (auch unauthentifizierte Laborwerkzeug-Ports) auf dem Host-Loopback offen.

## Regeln, die sich nicht ändern

Keine automatische Beförderung Cluster-übergreifender Kopien; kein Knotenersatz und kein Bootstrap, der Quorum oder Wiederherstellungsregeln umgeht; große Datenbank-Upgrades und die Wiederherstellung nach Totalausfall erfordern ausdrückliche Freigabe.
