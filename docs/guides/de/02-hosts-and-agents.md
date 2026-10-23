---
title: Hosts und Agenten einbinden
---

# Hosts und Agenten einbinden

[Übersicht](index.md) | Zurück: [Erste Schritte](01-getting-started.md) | Weiter: [Galera-Cluster anlegen](03-create-cluster.md)

Der Manager führt nie selbst Befehle auf Hosts aus. Auf jedem eingebundenen Host läuft ein kleiner **Agent** als vom Manager bereitgestellter Swarm-Global-Service. Er führt abgesicherte Befehle aus einer Positivliste aus (Volume anlegen, Datenbankdienst starten, `grastate.dat` lesen usw.) und fasst nur Objekte an, die die Installation selbst angelegt hat (Besitz-Label).

## Swarm anbinden

`POST /api/v1/swarms` mit dem Docker-Endpunkt (E4: `unix:///var/run/docker.sock` auf einem Swarm-Manager-Knoten), dann `POST /api/v1/swarms/{id}/sync`. Der Swarm steht auf `healthy`, `GET /api/v1/hosts` listet Knoten mit Rolle, CPUs, Speicher und Docker-Version [Lab-VMs] (3 Knoten). Die Anbindung an eine entfernte, gegenseitig authentifizierte Docker-API ist ein offener Entwurfspunkt (P04) [Ungetestet]. Eine Docker-Version außerhalb der Kompatibilitätsmatrix (nur Docker 29.x) wird markiert [Fakes].

## Voraussetzungen, die Sie selbst schaffen müssen

Der Manager legt sie noch nicht an und prüft sie nicht. Fehlendes führt zu verwirrenden Fehlern (siehe [Fehlersuche](09-troubleshooting.md)).

- Auf **jedem** Host: die Verzeichnisse `/var/lib/dbcm-agent` und die konfigurierte Volume-Basis (z. B. `/var/lib/dbcm/volumes`). Sonst lehnt Swarm den Agent-Task ab (`bind source path does not exist`).
- Mindestens ein eingebundener **Swarm-Manager-Knoten** mit Agent: Worker können Dienste nicht ändern, daher gehen Dienst- und Job-Befehle an den Agenten eines Manager-Knotens.
- Images auf jedem Knoten (Registry oder `docker save | docker load` wie im Labor). Images werden per Tag oder Digest referenziert.
- Die Manager-`settings` (Installations-ID, Volume-Basis, Agent-Image, Konten, Backup-Einstellungen). Ohne sie antworten die meisten Befehle mit `409 settings_incomplete`.
- Je Cluster: ein anhängbares Overlay-Netzwerk sowie die Passwort-Secrets für root, Monitoring und Backup, alle mit dem Label `dbcm.installation=<Installations-ID>`.

## Hosts einbinden

1. `POST /api/v1/hosts/{id}/enroll` je Host (`id` ist die Swarm-Knoten-ID). Erfordert die Berechtigung „Host ändern“, wird auditiert. Die Host-Seite der Oberfläche tut dasselbe.
2. `POST /api/v1/swarms/{id}/agents/deploy` legt bei Bedarf den Agent-Global-Service an und setzt das Einbindungs-Label je Host. Die Antwort listet `labeled`, `unlabeled` und `failed`.
3. Jeder Agent bindet sich selbst ein: Einmal-Token als Swarm-Secret, Pinning des CA-Fingerabdrucks, Zertifikatsanforderung (CSR) und Prüfung der Quelladresse. Danach spricht der Manager per **mTLS 1.3** mit dem Agenten.

Beobachtet: drei Agenten innerhalb von 2 s eingebunden, `agentIdentity: true` [Lab-VMs]. Die Host-Seite der Oberfläche lief nur gegen Fakes [Fakes].

## Host sperren

`POST /api/v1/hosts/{id}/revoke` markiert den Host als gesperrt und widerruft das Agentenzertifikat. Der Widerruf mit echtem Agenten, die Zertifikatserneuerung und die Verteilung der Sperrliste an Agenten wurden **nicht** geprüft [Ungetestet]; die Verteilung der Sperrliste ist eine bekannte Lücke.

## Secrets erneuern

Ein neues Datenbank-Secret für die Agenten erfordert ein Update des Agent-Dienstes. Nicht geprüft [Ungetestet].
