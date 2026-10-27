# Betriebssystem-Patch-Management (experimentell)

**[Fakes + Lab]** Das Patch-Management ([X15](../../KONZEPT.md#scope-extension), [ADR 0023](../../decisions/0023-os-patch-management.md)) ist implementiert und mit Fakes getestet. Der native Agent und die Paketmanager-Anbindung liefen außerdem einmal auf zwei echten Gast-Betriebssystemen im lokalen Lab (Debian 12 mit apt, AlmaLinux 9 mit dnf); was geprüft wurde und was nicht, steht in den [E10-Ergebnissen](../../../tests/p0/results/e10-patch.md). Die Managed-Manager-Laufzeit kann es hinter einem ausdrücklichen Schalter betreiben (`manager.patch.enabled`, Standard aus; siehe „Aktivieren“), geprüft mit Fakes und einem echten mTLS-Listener nur auf Loopback, nicht auf Lab-Hosts. Bei ausgeschaltetem Schalter antworten die Endpunkte mit 503. Es gibt keine Oberfläche dafür. Nicht auf Produktiv-Hosts verwenden.

## Aktivieren

Den Abschnitt in die Manager-Konfiguration aufnehmen (die mitgelieferte `deploy/manager.example.json` enthält ihn mit `enabled: false`):

```json
"patch": { "enabled": true, "agentPort": 7445, "pollIntervalSeconds": 300 }
```

Voraussetzung sind `auth.enabled` und `manager.agentTransport` (die Patch-Agenten nutzen dieselbe CA und das Manager-Clientzertifikat wie die Haupt-Agenten); sonst startet der Manager nicht. `agentPort` ist der Listen-Port der Patch-Agenten (Standard 7445; der mitgelieferte Metastore-Standard nutzt ebenfalls 7445 auf Loopback, auf einem Manager-Host den Patch-Agenten daher auf einem anderen Port starten und `agentPort` anpassen). `pollIntervalSeconds` (Standard 300, mindestens 60) bestimmt, wie oft der Leader nach Hosts mit abgelaufenem Report-Intervall und nach einer geänderten Zertifikats-Sperrliste sucht. Nur der Leader arbeitet; die Datensätze liegen im replizierten Metastore. Die Patch-Host-ID ist die Swarm-Node-ID des eingeschriebenen Hosts, und der Patch-Agent muss mit genau dieser `-host-id` laufen: der Manager bindet die Agent-Identität daran. Startet ein neuer Leader, werden Pläne im Zustand `running` als fehlgeschlagen mit unbekanntem Ausgang markiert (das Apply-Journal liegt im Speicher); an den Host wird dabei nichts gesendet.

Zertifikatssperre: Der Leader sendet die Sperrliste an jeden Patch-Host, der die aktuelle noch nicht bestätigt hat (Kommando `deny-list`, auch an Report-only-Agenten). Der Patch-Agent speichert sie in `patch-state.json` im Zustandsverzeichnis und weist ein vorgelegtes Zertifikat mit gelisteter Seriennummer ab, auch vor dessen Ablauf. Ein Host, der noch keine Liste bestätigt hat, ist nur durch die Zertifikatserneuerung geschützt.

## Funktionen

Ein kleiner Agent auf jedem Server meldet Betriebssystem und Kernel, installierte Pakete, verfügbare Updates, Sicherheitsupdates und ob ein Neustart nötig ist. Der Manager hält den letzten Bericht und eine begrenzte Historie je Host (200 Einträge), gruppiert Hosts und liefert Host- und Paketansichten über die API. Neue Sicherheitsupdates lösen je neuer Update-Version einmal eine Benachrichtigung aus. Die installierten Pakete fließen außerdem in das Schwachstellen-Inventar, sodass die CVE-Feeds ([Anleitung 15](15-vulnerability-response.md)) Betriebssystempakete abgleichen können. Eine Distribution ohne bekanntes Feed-Ökosystem erscheint als „unbekannt“, nie als sicher.

## Standard: nur berichten

Es wird nichts geändert, solange nicht alles zutrifft: Die wirksame Richtlinie des Hosts ist `approval_required`; es gibt einen Plan mit expliziter Paketliste und Simulation des Paketmanagers; ein Benutzer mit Update-Recht hat genau diesen Plan (Prüfsumme) freigegeben; die Zeit liegt im Wartungsfenster (falls Fenster gesetzt sind); der Host ist kein nicht koordiniertes Galera-Mitglied; der Agent wurde im Apply-Modus installiert. Wirksam ist die Richtlinie des Hosts, sonst die seiner Gruppe, sonst der Installationsstandard (kein feldweises Zusammenführen).

Felder: `mode` (`report_only` | `approval_required`), `securityOnly`, `windows` (Tage, `start` HH:MM, `durationMinutes`, IANA-`zone`, Fenster über Mitternacht funktionieren), `reboot` (`never` | `with_plan` | `auto_in_window`; alles außer `never` braucht ein Fenster), `reportEveryMinutes` (Standard 360).

## Rollen

Betrachter lesen. Operatoren lesen, holen einen frischen Bericht und schlagen Pläne vor. Freigeben, Anwenden, Richtlinien, Gruppen und Hosts ändern sowie Agenten installieren braucht die Rolle Administrator (oder eine eigene Rolle mit `patch`-Update/Create).

## Typischer Ablauf (API)

1. `PUT /api/v1/patch/groups/web` und `PUT /api/v1/patch/hosts/h1` (Name, Gruppe, optionale Richtlinie).
2. `POST /api/v1/patch/hosts/h1/poll`, dann `GET /api/v1/patch/hosts/h1` und `/api/v1/patch/packages`.
3. `POST /api/v1/patch/hosts/h1/plans` mit `{"securityOnly": true}`. Die Antwort enthält Pakete, Simulation und Prüfsumme.
4. `POST /api/v1/patch/plans/{id}/approve` mit der Prüfsumme, danach `POST /api/v1/patch/plans/{id}/apply`.
5. `GET /api/v1/patch/hosts/h1/history` prüfen. Blieben Updates offen, steht das in `remaining`.

## Neustarts

Ein Neustart passiert nur, wenn die Richtlinie ihn erlaubt. Bei `with_plan` muss der Plan den Neustartschritt enthalten (`"reboot": true`); seine Freigabe ist die Freigabe des Neustarts. Bei `auto_in_window` folgt auf ein freigegebenes Anwenden ein Neustart, wenn der Host „Neustart nötig“ meldet. Zusätzlich sind das Fenster und eine zweite Guard-Prüfung unmittelbar vor dem Neustart nötig. Kann ein Host nicht sagen, ob ein Neustart nötig ist (Alpine, Arch), meldet er `unknown`; `auto_in_window` startet bei `unknown` nicht neu.

## Galera-Mitglieder

Pakete auf einem Host mit Galera-Mitglied anzuwenden oder ihn neu zu starten wird abgelehnt, solange nicht die rollierende Prozedur des Cluster-Planers genau diese Aktion koordiniert (`coordination` im Plan). Lässt sich die Mitgliedschaft nicht bestimmen, wird abgelehnt. Mit `manager.patch.enabled` ist der Guard an die geführte Host-Wartung des Planers angebunden (Abschnitt „Host-Wartung“ im [Betriebshandbuch](11-operator-runbook.md)): Host in Wartung setzen (`POST /api/v1/hosts/{id}/maintenance`, stoppt seine Galera-Knoten nacheinander sauber, nachdem der Rest des Clusters als Synced geprüft wurde), warten bis der Host bereit ist, dann den Plan mit `"coordination": "<ID der Wartungsoperation>"` aus `maintenance.operations` des Hosts vorschlagen. Apply und Reboot sind nur erlaubt, solange der Host in Wartung ist, jede am Host gelistete Operation erfolgreich war und jeder Cluster mit einem Knoten auf dem Host abgedeckt ist; sonst werden sie abgelehnt. Nach dem Patchen (und Neustart) die Wartung selbst beenden (`DELETE /api/v1/hosts/{id}/maintenance`), damit die Knoten wieder starten. Ein Host ohne Cluster-Knoten gilt nicht als Mitglied. Der Guard ist nur mit Fake-Agenten geprüft, nicht gegen einen echten Cluster.

## Nativen Agenten installieren

Der Administrator sendet eine Installationsanfrage (`POST /api/v1/patch/install/plans`): Host, Modus, HTTPS-URL und SHA-256 der Agent-Binärdatei, Manager-URL und CA-Fingerabdruck. Der Manager führt eine schreibgeschützte SSH-Vorprüfung aus (Linux, unterstützte Architektur, systemd, root oder sudo, Paketmanager, kein vorhandener Agent) und liefert das genaue Skript, die systemd-Unit und eine Prüfsumme. Der SSH-Schlüssel ist nur zur Eingabe sichtbar und wird verschlüsselt gespeichert. Die Ausführung mit der Prüfsumme wiederholt die Vorprüfung, lädt die Binärdatei, **prüft deren SHA-256 auf dem Host vor der Installation**, legt den Benutzer an und liest das einmalige Enrollment-Token von stdin. Zwei Modi: `report` (Benutzer `dbcm-patch`, abgesicherte Unit, kann nichts anwenden) und `apply` (root, `-allow-apply`). Eine Installation über einen vorhandenen Agenten wird abgelehnt. Entfernen: `dbcm-patch-agent.service` stoppen und deaktivieren, Unit, `/usr/local/bin/dbcm-agent`, `/etc/dbcm-patch`, `/var/lib/dbcm-patch` und den Benutzer löschen (dieselben Schritte enthält `patch.UninstallScript`).

Zum rein lesenden Ausprobieren: `dbcm-agent patch -once` gibt einen Bericht als JSON aus und ändert nichts.

## Grenzen

Auf echten Hosts nicht geprüft: SSH-Transport, Enrollment und mTLS des Patch-Agenten gegen einen echten Manager, yum, zypper, apk und pacman (nur Parser und Fakes), der Planer gegen einen echten Cluster, Verhalten bei einem echten Manager-Failover. Der mTLS-Pfad vom Manager zum Agenten, die Sperrliste, die dauerhafte Wiederholungsunterdrückung und der Wartungs-Guard wurden mit Fakes und Loopback-Listenern getestet.

Wiederholte Schritte werden dauerhaft unterdrückt: Der Agent vermerkt einen Schritt, bevor er ihn ausführt. Starb Agent oder Host während eines Schritts, wird eine Wiederholung mit „step interrupted“ abgelehnt, der Plan als fehlgeschlagen markiert, und der Host-Report ist vor einem neuen Plan zu prüfen. Der Agent behält die letzten 1000 Schritte. Das Apply-Journal des Managers liegt nur im Speicher (siehe „Aktivieren“). Der CVE-Abgleich für RPM-Distributionen (AlmaLinux, Rocky, Red Hat, CentOS, Fedora, Oracle Linux, SUSE) nutzt die `rpmvercmp`-Ordnung mit Epoch und Release; Advisories gibt es nur, wenn ein konfigurierter Feed sie liefert (OSV ist nur für AlmaLinux und Rocky Linux abgebildet). Die SSH-Installationsrouten antworten in der Managed-Laufzeit vorerst mit 503 (versiegelter Entwurfsspeicher und Token-Quelle sind nicht angebunden); den Agenten wie unten beschrieben von Hand installieren. Es gibt keine Oberfläche; API und diese Anleitung sind die Schnittstelle.
