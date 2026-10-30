---
title: Optionale Module
---

# Optionale Module [Nur Fakes und Manager im selben Prozess]

Optionale Funktionen sind Module ([ADR 0030](../../decisions/0030-optional-modules.md)): OS-Patchverwaltung, Host-Backups (restic), Backup-Kopien, Image-Build, Image-Composer, Image-Updates, Schwachstellen und KI-Antwort, Server-Bootstrap, SSH-Provisionierung und Kubernetes. Eine Administratorin oder ein Administrator schaltet jedes Modul im laufenden Betrieb ein oder aus; ein Neustart ist nicht nötig.

## Ein Modul schalten

Öffnen Sie **Einstellungen > Optionale Module** (ändern: Administrator, lesen: jede angemeldete Person).

- **Einschalten:** Schalter aktivieren. Geht das nicht, nennt die Seite den Grund: ein Konfigurationsschlüssel fehlt (etwa `manager.agentTransport` für die Patchverwaltung) oder ein benötigtes Modul ist aus (der Composer braucht den Image-Build).
- **Ausschalten:** Schalter deaktivieren. Das wird abgelehnt, solange ein anderes eingeschaltetes Modul es braucht; schalten Sie zuerst dieses aus.
- Jede Karte zeigt, wer das Modul wann geschaltet hat. Jede Änderung wird protokolliert.

Ein ausgeschaltetes Modul startet keine neue Arbeit, seine Navigationseinträge verschwinden und seine API-Routen antworten `404 module_disabled`. **Es wird nichts gelöscht:** Hosts, Pläne, Verlauf, versiegelte Zugangsdaten und Einstellungen bleiben und sind beim erneuten Einschalten wieder da. Eine bereits laufende Operation wird beendet, nicht abgebrochen.

## Konfigurationsdatei

Die Abschnitte `manager.patch`, `imageUpdate`, `imageComposer`, `serverBootstrap`, `imageBuild`, `vuln`, `experimentalProvisioning` und `experimentalKubernetesProvisioning` funktionieren weiter. Sie bestimmen jetzt den Anfangszustand eines Moduls, das noch nie geschaltet wurde; sobald Sie es auf der Seite schalten, gilt der gespeicherte Zustand. Host-Backups und Backup-Kopien sind eingeschaltet, wenn `manager.backup` konfiguriert ist. Was ein Modul voraussetzt (Agent-Transport, `imageBuild`, das geprüfte Agent-Release, Intervalle), bleibt in der Datei.

## API

`GET /api/v1/modules` listet die Module mit `enabled`, `missingConfig`, `missingModules`, `dependents`, `by` und `at`. `PUT /api/v1/modules/{id}` mit `{"enabled": true}` schaltet eines; `409 module_blocked` bedeutet, dass die Gründe aus der Liste gelten.
