---
title: Dienste (Entwurf)
---

# Dienste (Entwurf)

[Handbuchindex](index.md) | Zurück: [Konfigurationsreferenz](12-configuration-reference.md)

**Stand: nur [Fakes].** Das Dienstetyp-Framework (Arbeitspaket W07, Anforderungen X03, X06 und X07 in [KONZEPT.md](../../KONZEPT.md)) existiert als Code mit Unit-Tests. Es lief nie gegen einen Swarm, Docker oder Traefik, und es gibt noch keine Oberfläche dafür. Diese Seite ist ein Platzhalter, damit niemand sie für eine nutzbare Funktion hält.

## Was es gibt

Zwei Dienstetypen, geplant von reinem Code (`internal/service`) und über die Manager-API erreichbar:

- **container**: ein generischer Docker-Dienst mit festgelegtem Image, Umgebung, Swarm-Secrets, Netzwerken, verwalteten lokalen Volumes, Ressourcen, Platzierung und Neustart-Richtlinie.
- **traefik**: ein Traefik-Dienst mit einem typisierten Modell aus Entry Points, ACME-Zertifikatsresolvern, Routern, Services (Backends) und Middlewares. Der Manager rendert statische und dynamische Konfiguration als YAML zur Prüfung.

Regeln der Typen: Images müssen festgelegt sein (kein `latest`); Umgebungsvariablen, die nach Geheimnissen aussehen (`*_PASSWORD`, `*_TOKEN`, ...), werden abgelehnt und müssen ein Swarm-Secret mit der `_FILE`-Konvention nutzen; Volumes sind verwaltete lokale Volumes mit Identitätsmarker, nie Host-Pfade, und brauchen einen festen Host und ein Replikat. Gerenderte Konfiguration enthält nie Geheimnisse.

## Ablauf

1. `POST /api/v1/services/plan` zeigt den Plan (Schritte, erzeugte Artefakte, Warnungen, Blocker), ohne etwas zu ändern.
2. `POST /api/v1/services` legt eine Operation an und wartet. Jeder zustandsändernde Schritt braucht eine Freigabe, die an den Digest der geplanten Operation gebunden ist.
3. `POST /api/v1/operations/{id}/approve` mit diesem Digest lässt sie laufen. Die Freigabe braucht die Berechtigung `create` (bei Löschung `delete`) auf die Ressource `service` im Swarm.
4. `DELETE /api/v1/services/{id}` plant das Entfernen auf dieselbe Weise; Volumes und Daten bleiben erhalten, außer `removeVolumes` ist gesetzt.

Berechtigungen sind standardmäßig verweigert. Die Presets Viewer und Operator können Dienste nur lesen; das Administrator-Preset darf alles. Jede Entscheidung wird protokolliert.

## Bekannte Grenzen

- **Veröffentlichte Ports sind noch nicht möglich**: Die Dienst-Spezifikation des Agenten hat kein Port-Feld. Ein Plan mit veröffentlichtem Port wird mit einem Blocker angezeigt und kann nicht gestartet werden. Das gilt auch für die Entry Points von Traefik.
- **Konfigurationsdateien können noch nicht ausgeliefert werden**: Der Agent hat dafür keinen Befehl; die dynamische Traefik-Konfiguration (Router, Services, Middlewares) ist daher nur eine Vorschau und blockiert den Plan ebenfalls.
- Keine Gesundheitsbeobachtung angebunden, keine Update-Strategie, keine Sicherung von Dienst-Volumes, und Galera ist kein Dienstetyp (es behält sein eigenes Paket).
- Keine Oberfläche, keine CLI, keine Experten- oder Exportansicht außer den Artefakten in der Planantwort.
