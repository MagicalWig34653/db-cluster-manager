---
title: Sicherheitsmodell
---

# Sicherheitsmodell

[Übersicht](index.md) | Zurück: [Totalausfall](07-full-outage-recovery.md) | Weiter: [Fehlersuche](09-troubleshooting.md)

Beschrieben sind Entwurf und Prüfstand. Es gab keine externe Sicherheitsprüfung, und nichts wurde gegen ein feindliches Netz getestet.

## Authentifizierung und Tokens

- **Nur Passkeys.** Es gibt keine Passwörter für die Oberfläche. Sitzungen liegen serverseitig, mit Leerlaufzeit 30 Minuten und Höchstdauer 12 Stunden (Standard); Abmeldung und Wiederherstellung beenden sie. Strikte `Origin`-Prüfung schützt zustandsändernde Anfragen (CSRF). [Fakes] mit Software- und virtuellem Browser-Authenticator; [Lab-VMs] nur für den API-Ablauf.
- **Einrichtungstoken:** wird einmal beim Start ausgegeben, solange kein Administrator existiert; je Client-IP begrenzt; nach dem ersten Administrator dauerhaft deaktiviert.
- **Wiederherstellungscodes:** zehn Einmalcodes bei der Einrichtung, einmal angezeigt, als Hashes gespeichert. Das Einlösen entfernt die Passkeys des Kontos, beendet alle Sitzungen und öffnet eine kurze Wiederherstellungssitzung (10 Minuten Leerlauf, 15 absolut), die nur das Registrieren eines neuen Passkeys erlaubt. Versuche sind je IP und Konto begrenzt (fünf Fehlversuche in 15 Minuten sperren 15 Minuten, verdoppelnd bis 24 Stunden); jeder Fehler liefert denselben Antworttext. [Fakes].
- **Wiederherstellungs-URL-Tokens** (`/auth/recovery/token`): Der Befehl zum Ausstellen existiert noch nicht, der Weg ist nicht nutzbar [Ungetestet].

## Autorisierung und Audit

Standardmäßig verboten. Preset-Rollen: Viewer, Operator, Datenbankadministrator, Administrator (plus Systemrolle des Managers); eigene Rollen sind möglich. Berechtigungen gelten je Ressourcentyp und Aktion in einem Geltungsbereich (Swarm, Host, Cluster, Backup usw.). Ressourcen, die ein Aufrufer nicht lesen darf, antworten 404. Zerstörerische Operationen (Cluster löschen, Inspektions-Wiederherstellung, Totalausfall-Wiederherstellung) warten auf eine **an einen Digest gebundene Freigabe**: Der Digest ist der SHA-256 des gespeicherten Plans, Sie geben also genau das Gezeigte frei. Das Löschen eines Clusters braucht die Berechtigung Löschen, die nur das Administrator-Preset hat. Jede Entscheidung wird auditiert; im verwalteten Modus in einem persistenten replizierten Speicher (im Prototyp-Modus ins Log). [Fakes]; die Digest-Prüfung und `409 approval_mismatch` liefen für die Inspektions-Wiederherstellung auch auf Lab-VMs [Lab-VMs].

## Kanal Manager-Agent (mTLS)

Agenten binden sich mit einem Einmal-Token (als Swarm-Secret), Pinning des CA-Fingerabdrucks, CSR und Prüfung der Quelladresse ein. Danach nutzen beide Seiten TLS 1.3 mit Clientzertifikaten; Agentenbefehle sind durch einen Term abgesichert, idempotent und gegen eine Besitz-Positivliste geprüft, ein Agent lehnt also Objekte ab, die die Installation nicht angelegt hat. Einbindung und mTLS-Transport liefen auf Lab-VMs [Lab-VMs]. Widerruf, Erneuerung und Verteilung der Sperrliste wurden nicht geprüft [Ungetestet].

## Secrets und Daten im Ruhezustand

Datenbankpasswörter liegen in Swarm-Secrets, nie in der API oder in versionierten Dateien. Benachrichtigungs-Secrets sind nur schreibbar und verschlüsselt gespeichert (AES-GCM). Der Datenschlüssel des Managers wird beim ersten Start im Datenverzeichnis angelegt und **muss außerhalb des Systems auf jedes Replikat kopiert werden**: Die Schlüsselverteilung ist nicht gelöst. Backups sind mit einem Recovery-Key verschlüsselt, den Sie außerhalb des Clusters aufbewahren müssen; ohne ihn sind Backups unlesbar.

## Bekannte Schwächen

- Der Metastore-Transport zwischen Manager-Replikaten hat kein TLS.
- Backup-Manifeste enthalten Klartext-Chunk-Digests (Frage zu ADR 0009).
- Oberfläche und API haben kein eigenes TLS; nutzen Sie einen TLS-Reverse-Proxy.
- Lima leitet Gast-Listener auf den Host-Loopback weiter: Im Labor waren das unauthentifizierte Metastore-Werkzeug und Agentenports auf dem Host erreichbar. Nur Labor, aber keine Laborwerkzeuge auf gemeinsam genutzten Rechnern betreiben.
- TLS für Datenbankclients gibt es nur im Proxy; TLS zwischen Knoten und zwischen Proxy und Knoten gibt es nicht. Der Manager konfiguriert kein TLS zwischen Galera-Knoten; die Verschlüsselung des vom Betreiber angelegten Overlay-Netzwerks (`--opt encrypted`) liegt bei Ihnen und wurde nicht getestet.
