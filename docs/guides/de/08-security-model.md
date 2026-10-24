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

Agenten binden sich mit einem Einmal-Token (als Swarm-Secret), Pinning des CA-Fingerabdrucks, CSR und Prüfung der Quelladresse ein. Danach nutzen beide Seiten TLS 1.3 mit Clientzertifikaten; Agentenbefehle sind durch einen Term abgesichert, idempotent und gegen eine Besitz-Positivliste geprüft, ein Agent lehnt also Objekte ab, die die Installation nicht angelegt hat. Einbindung und mTLS-Transport liefen auf Lab-VMs [Lab-VMs]. Die Verteilung der Sperrliste an Agenten und der Widerruf der Zertifikate eines Manager-Replikats sind implementiert und auf Loopback getestet [Fakes]; Widerruf und Erneuerung mit echten Agenten auf VMs wurden nicht geprüft [Ungetestet]. Siehe [Hosts und Agenten](02-hosts-and-agents.md).

## Secrets und Daten im Ruhezustand

Datenbankpasswörter liegen in Swarm-Secrets, nie in der API oder in versionierten Dateien. Benachrichtigungs-Secrets sind nur schreibbar und verschlüsselt gespeichert (AES-GCM). Der Datenschlüssel des Managers wird beim ersten Start im Datenverzeichnis angelegt. Für mehrere Replikate setzen Sie `manager.keyring`: Der Schlüssel liegt dann nur verpackt für die Identität jedes registrierten Replikats (`<dataDir>/replica.key`) und für den Backup-Recovery-Key vor; ein Administrator registriert jedes Replikat, indem er den mit `manager -print-replica-key` ausgegebenen öffentlichen Schlüssel auf einem anderen Weg vergleicht (`PUT /settings/replicas/{id}/data-key-recipient`, nur Voter, auditiert), und ein Replikat ohne Schlüssel gibt die Leadership ab ([ADR 0013](../../decisions/0013-replica-keys-peer-tls-and-revocation.md)) [Fakes]. Ohne `keyring` muss die Schlüsseldatei weiterhin von Hand auf jedes Replikat kopiert werden. Es gibt keine Schlüsselrotation und keine Neuverschlüsselung; das Zurückziehen einer Registrierung nimmt keine Schlüssel zurück, die ein Replikat schon entpackt hat. Backups sind mit einem Recovery-Key verschlüsselt, den Sie außerhalb des Clusters aufbewahren müssen; ohne ihn sind Backups unlesbar.

## Bekannte Schwächen

- Der Metastore-Transport zwischen Manager-Replikaten ist unverschlüsseltes TCP, solange `manager.peerTLS` nicht konfiguriert ist (TLS 1.3, gegenseitig, Peer-CA aus `manager -issue-peer-certs`) [Fakes]. Peer-Zertifikate erneuert das Produkt nicht, und die Peer-CA halten Sie offline.
- Backup-Manifeste im Format 1 enthalten Klartext-Chunk-Digests; Format 2 nutzt HMACs mit Schlüssel [Fakes]. Alte Backups behalten das schwächere Format.
- Oberfläche und API unterstützen ACME oder importierte TLS-Zertifikate [Fakes], siehe [Manager-HTTPS](17-manager-tls.md). HTTP außerhalb numerischer Loopback-Adressen braucht ausdrücklich `manager.tls.allowPlainHttp` für Ihren eigenen Proxy.
- Lima leitet Gast-Listener auf den Host-Loopback weiter: Im Labor waren das unauthentifizierte Metastore-Werkzeug und Agentenports auf dem Host erreichbar. Nur Labor, aber keine Laborwerkzeuge auf gemeinsam genutzten Rechnern betreiben.
- TLS für Datenbankclients gibt es nur im Proxy; TLS zwischen Knoten und zwischen Proxy und Knoten gibt es nicht. Der Manager konfiguriert kein TLS zwischen Galera-Knoten; die Verschlüsselung des vom Betreiber angelegten Overlay-Netzwerks (`--opt encrypted`) liegt bei Ihnen und wurde nicht getestet.
