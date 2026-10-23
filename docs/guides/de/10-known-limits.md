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
- **Manager-HA:** Ein Multi-Replika-Manager lief nicht auf VMs. Der Metastore-Transport hat kein TLS, der Datenschlüssel muss von Hand auf Replikate kopiert werden, die Sperrliste der Agenten wird nicht verteilt. `deploy/stack.yaml` ist keine HA-Bereitstellung.
- **Paketierung:** Container- und Job-Image wurden vom Projekt nie gebaut; der Release-Workflow lief nie; Stack- und Compose-Datei für den verwalteten Modus (ein Replikat, an einen Swarm-Manager-Knoten gebunden, Docker-Socket eingebunden) wurden nur statisch geprüft und nie bereitgestellt.
- **Datenbankzugriff:** Der Manager stellt keine Proxys bereit; keine redundanten Proxys, kein VIP, kein Knoten-TLS oder Proxy-zu-Knoten-TLS. TLS für Clients gibt es nur im Proxy, ohne Zertifikatsausstellung (kein ACME).
- **Betrieb:** keine Drift-Behandlung, kein Wartungsmodus, kein Ersatzhost-Ablauf, kein Update-Ablauf, keine Migration, kein phpMyAdmin, kein PITR. Beitretende Knoten nutzen immer SST.
- **Backups:** keine geprüfte Vollwiederherstellung produktiver Daten; Wiederherstellung in einen neuen Cluster, logisches Backup und der Zeitplaner liefen nicht auf VMs; die Manager-Konfigurationssicherung ist eine Bibliothek ohne Planer oder API. Manifeste enthalten Klartext-Chunk-Digests. Die Wiederherstellungsprüfung vergleicht keine Tabelleninhalte.
- **Monitoring:** kein Ereignis und keine Benachrichtigung bei Totalausfall; für viele Standard-Alarmreihen fehlt ein echter Sammler; Benachrichtigungskanäle gegen echte Empfänger ungetestet.
- **Autorisierung:** Die Freigabe der Totalausfall-Wiederherstellung nutzt die Berechtigung Wiederherstellen. Echte Hardware-Authenticator, Safari/Firefox und Screenreader ungetestet.
- **Löschen:** Das Löschen eines Clusters lässt Host-Verzeichnisse lokaler Volumes und Backups im Ziel bewusst bestehen und lief nur gegen Fakes.
- **Belastbarkeit:** Ressourcenerschöpfung und Kapazitätsverweigerung, Neustart während der Bereinigung, VM-Stromausfall und geordnetes Leeren eines Hosts wurden nicht getestet.
- **Laborrisiko:** Lima legt Gast-Listener (auch unauthentifizierte Laborwerkzeug-Ports) auf dem Host-Loopback offen.

## Regeln, die sich nicht ändern

Keine automatische Beförderung Cluster-übergreifender Kopien; kein Knotenersatz und kein Bootstrap, der Quorum oder Wiederherstellungsregeln umgeht; große Datenbank-Upgrades und die Wiederherstellung nach Totalausfall erfordern ausdrückliche Freigabe.
