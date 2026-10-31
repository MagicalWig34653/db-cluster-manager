---
title: Frischen Server aufnehmen
---

# Frischen Server aufnehmen [Im Labor mit einem Harness auf Debian 12 und AlmaLinux 9 gelaufen; API und Assistent nicht auf VMs]

Der Bootstrap-Assistent ([ADR 0027](../../decisions/0027-fresh-server-bootstrap.md), Anforderung X18) macht einen frisch installierten Linux-Server, den Sie administrieren, zu einem verwalteten Host. Sie geben einmal Root-Zugang (oder sudo) per SSH ein; der Manager prüft den Server, zeigt einen Plan aus geprüften Skripten und ändert nichts, bis Sie den SSH-Hostschlüssel bestätigen und den Plan freigeben. Er baut auf dem SSH-Plan-Ablauf aus [Anleitung 19](19-provisioning.md) und den Agenten aus [Anleitung 24](24-agent-runtimes.md) auf.

**Stand.** Plan-Generator, Ausführung, API und Assistent sind gegen einen simulierten Host im Arbeitsspeicher getestet. Im Laborlauf [E17](../../../tests/p0/results/e17-bootstrap.md) liefen Plan und Ausführung mit dem echten SSH-Runner gegen eine frische Debian-12-VM (Schlüssel-Login) und eine frische AlmaLinux-9-VM (Passwort-Login, einschließlich der separat freigegebenen Absicherung): Firewall, Docker aus dem Hersteller-Repository, Swarm-Init und die Absicherung funktionierten nach drei Korrekturen (Speicher-Parsing, gpg, ein eigener Docker-rpm-Schlüssel-Fingerabdruck). Ubuntu, Rocky, Alpine, der Native-Agent-Schritt, die Cluster-Beitritts-Schritte und der Weg über HTTP-API und Assistent liefen nicht auf einem echten Host. Der Managed-Manager verdrahtet das Backend hinter `manager.serverBootstrap.enabled` (Standard aus, siehe „Aktivieren“); bei ausgeschaltetem Schalter antworten die Routen mit 503. Die Verdrahtung ist nur mit Fakes geprüft. Richten Sie ihn nicht auf einen Server, den Sie nicht neu aufsetzen können.

## Unterstützte Systeme

Debian 12/13, Ubuntu 22.04/24.04, AlmaLinux 9/10, Rocky Linux 9/10 und Alpine 3.19-3.22 auf amd64 oder arm64. Alles andere wird abgelehnt. Unter Alpine gibt es den nativen Agenten, die Firewall-Option und automatische Updates nicht (sie brauchen systemd, ufw/firewalld und apt/dnf).

## Den Assistenten benutzen

Öffnen Sie **Frischer Server** in der Navigation (nur Administrator).

1. **Host und Anmeldung.** Benennen Sie die Aufnahme, geben Sie Adresse, Anmeldebenutzer (meist `root`) und Passwort oder privaten Schlüssel an. Beides ist nur schreibbar: Es wird für diesen Lauf benutzt, nie gespeichert, protokolliert oder erneut angezeigt und nach dem Lauf, beim Abbrechen, nach falschem Fingerabdruck und nach einer Stunde ohne Aktivität gelöscht.
2. **Was der Server werden soll.** Wählen Sie den anzulegenden Dienstbenutzer (Standard `dbcm`), die Container-Laufzeit (Docker aus dem Hersteller-Repository oder keine), optional Firewall und automatische Sicherheitsupdates, den nativen und/oder den Container-Agenten, die Fähigkeiten Patch und Restic (sie brauchen den nativen Agenten; Restic braucht erlaubte Pfade) und den Cluster (keiner, Swarm init/join; k3s ist eine blockierte Vorschau). Widersprüche werden aufgelistet, bevor etwas kontaktiert wird.
3. **SSH-Hostschlüssel bestätigen.** Der Manager liest den Hostschlüssel genau dieses Servers ohne Anmeldung und zeigt Typ und Fingerabdruck. Vergleichen Sie ihn mit der Konsole Ihres Anbieters oder mit `ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub` auf dem Server und tippen Sie ihn ein. Ein falscher Fingerabdruck bricht den Entwurf ab und löscht die Zugangsdaten; Ihre Anmeldung wird vor diesem Schritt nicht benutzt.
4. **Plan prüfen.** Die Vorprüfung (nur lesend) läuft und der Plan erscheint: Ergebnisse (blockierend, Warnung, Hinweis), jeder Schritt mit Skript und Prüfsumme, seiner lesenden Prüfung und seinem Aufräumplan. Was der Server schon erfüllt, ist markiert. Ein blockierter Plan kann nicht freigegeben werden. Die Freigabe erfolgt mit der Plan-Prüfsumme.
5. **Ausführen und Fortschritt.** *Ausführen* führt die freigegebenen Schritte der Reihe nach aus. Jeder Schritt wird journalisiert; ein bereits erfüllter Schritt wird übersprungen; nach einem Fehler pausiert der Lauf und *Fortsetzen* macht nach dem letzten fertigen Schritt weiter (ein Schritt mit unbekanntem Ergebnis wird zuerst neu geprüft). Die Fortschrittsliste aktualisiert sich während des Laufs.
6. **Absicherung (separate Freigabe).** Haben Sie den eigenen Schlüssel und/oder das Abschalten des Root-Logins gewählt, warten diese Schritte nach der Hauptphase. Sie installieren einen eigenen Schlüssel für den Dienstbenutzer, prüfen eine Anmeldung damit über eine neue Verbindung, legen den privaten Schlüssel im Manager ab, schreiben dann eine sshd-Drop-in-Datei, die den Root-Login abschaltet, mit automatischem Zurücksetzen nach drei Minuten, und heben das Zurücksetzen erst auf, wenn eine neue Verbindung mit dem eigenen Schlüssel funktioniert. Prüfen Sie sie und geben Sie die Absicherungs-Prüfsumme separat frei.

## Wie ein Schritt aussieht

Jeder Schritt ist ein Shell-Skript mit Markierungszeile, SHA-256-Prüfsumme, Idempotenz-Schutz (eine Markerdatei in `/var/lib/dbcm-bootstrap` plus die echte Bedingung), lesender Prüfung und Aufräumskript. Der Aufräumplan macht nur rückgängig, was der Schritt hinzugefügt hat (Repository-Dateien, sudoers-Drop-in, von ihm geöffnete Firewall-Regeln, das sshd-Drop-in), und bereinigt, entfernt erzwungen oder löscht nie Daten; er nennt, was absichtlich bleibt (Docker Engine, Benutzer, Pakete). Das Produkt führt den Aufräumplan nie selbst aus; das tun Sie nach Prüfung.

Geheimnisse: Das Enrollment-Token und das Swarm-Beitritts-Token laufen über die Standardeingabe, nie in einer Kommandozeile. Plan und jede API-Antwort enthalten keine Zugangsdaten.

## Aktivieren

Abschnitt ergänzen (die mitgelieferte `deploy/manager.example.json` enthält ihn mit `enabled: false`):

```json
"serverBootstrap": { "enabled": true, "sweepSeconds": 60,
  "agentRelease": { "version": "0.1.0", "binaryUrl": "https://releases.example.org/dbcm-agent-linux-amd64", "sha256": "<64 hex>" } }
```

Der Schlüssel heißt `serverBootstrap`, weil `manager.bootstrap` bereits den Metastore-Cluster startet. Er braucht `auth.enabled`, `manager.agentTransport` (der installierte native Agent schreibt sich dort ein) und das geprüfte Release des nativen Agenten; sonst startet der Manager nicht. Entwürfe und Zugangsdaten werden mit dem Datenschlüsselring versiegelt im replizierten Metastore gehalten: Das Entwurfsdokument enthält Plan und festgehaltenen Host-Schlüssel, aber keine Zugangsdaten; diese liegen in einem zweiten Dokument, das nach dem Lauf, beim Abbruch, bei falschem Fingerabdruck und nach Ablauf der einen Stunde gelöscht wird (der Leader räumt alle `sweepSeconds` auf; ein fehlgeschlagenes Löschen wird wiederholt). Nur der Leader führt einen Bootstrap aus, und ein Lauf, der nach einem Leaderwechsel endet, schreibt kein Ergebnis. Nur Administratoren.

## Grenzen

- Nur Simulation; Skripte sind mit `sh -n` auf Syntax geprüft, aber nie auf einem echten System gelaufen. Paketnamen, Repository-URLs, Dienstnamen, Firewall-Befehle und der fest hinterlegte Docker-Schlüssel-Fingerabdruck sind ungeprüfte Annahmen.
- Entwürfe sind dauerhaft (ADR 0028), das Schrittjournal liegt im Speicher. Nach Neustart oder Failover markiert der neue Leader einen Lauf ohne aufgezeichnetes Ende als `unknown`; erneutes Ausführen setzt ihn fort, und jeder Schritt wird auf dem Host geprüft, bevor er wiederholt wird (als erfolgreich vermerkte Schritte werden wiederhergestellt, der laufende Schritt wird neu geprüft). Die Verdrahtung lief nur gegen Fakes, nie über zwei echte Manager-Prozesse.
- Das Enrollment-Token gibt es nur für einen Host mit eingeschriebenem Host-Datensatz, dessen Node-ID der Host-ID des Bootstraps entspricht. Ein Server, der noch kein Swarm-Node ist, hat keinen; der Schritt für den nativen Agenten scheitert dann mit „enrollment token unavailable“, bis der Node bekannt ist (erst dem Swarm beitreten, synchronisieren, einschreiben, dann erneut ausführen). Das ist eine bekannte Lücke und noch nicht auf einem Lab-Host geprüft.
- Der Schritt für den Container-Agenten schreibt den Node ein und führt die vorhandene Agent-Bereitstellung seines Swarms aus (Labels, Agent-Dienst); ein gesperrter Host wird nie erneut eingeschrieben.
- Ein Host pro Aufnahme; k3s ist blockiert; eine vorhandene Docker Engine bleibt unverändert; eine Firewall filtert von Docker veröffentlichte Ports nicht.
- Ein Neustart des Servers in den drei Minuten nach dem Abschalten des Root-Logins verwirft das automatische Zurücksetzen; dann muss der eigene Schlüssel funktionieren.

**Modulschalter.** Seit [ADR 0030](../../decisions/0030-optional-modules.md) ist diese Funktion ein Modul, das eine Administratorin im Betrieb schaltet ([Anleitung 28](28-modules.md)). Der Konfigurationsabschnitt oben legt nur den Anfangszustand fest; die Routen einer ausgeschalteten Funktion antworten `404 module_disabled` (nicht 503).
