---
title: Benutzerhandbuch (Deutsch)
---

# Benutzerhandbuch (Deutsch)

Diese Anleitungen beschreiben, was die Software **heute** kann. Es handelt sich um Vorabsoftware: Nichts lief auf einem Produktiv-Swarm, der erste Swarm-Test hat nicht begonnen. Jede Funktion trägt eine Prüfstufe, entnommen aus [STATUS.md](../../STATUS.md) und den Laborergebnissen in [tests/p0/results](../../../tests/p0/results/e4-manager-galera.md).

English version: [User guides (English)](../en/index.md). Technische Details (englisch): [INSTALL.md](../../INSTALL.md), [KONZEPT.md](../../KONZEPT.md), [api/openapi.yaml](../../../api/openapi.yaml).

## Prüfstufen

| Stufe | Bedeutung |
| --- | --- |
| **[Lab-VMs]** | Lief mit echtem Manager, echten mTLS-Agenten und echtem MariaDB 11.4 auf dedizierten Lima-VMs (arm64), ein- bis wenige Male beobachtet (Experimente E1-E6). Kein Produktionsnachweis. |
| **[Fakes]** | Implementiert und nur gegen Fakes unit-getestet (Fake-Docker, Fake-Agenten, In-Memory-Speicher, `httptest`). Nie gegen die echte Umgebung gelaufen. Bis zu einem Laborlauf nicht vertrauen. |
| **[Ungetestet]** | Entwurf oder Codepfad ohne jeden Test des echten Verhaltens, oder nicht implementiert. |

Haben Teile einer Funktion verschiedene Stufen, sind sie einzeln gekennzeichnet.

## Anleitungen

1. [Erste Schritte und Installation](01-getting-started.md)
2. [Hosts und Agenten einbinden](02-hosts-and-agents.md)
3. [Galera-Cluster anlegen](03-create-cluster.md)
4. [Zustand und Alarme](04-health-alerts.md)
5. [Backups und Wiederherstellung](05-backup-restore.md)
6. [Knotenersatz](06-node-replacement.md)
7. [Wiederherstellung nach Totalausfall mit Freigabe](07-full-outage-recovery.md)
8. [Sicherheitsmodell](08-security-model.md)
9. [Fehlersuche](09-troubleshooting.md)
10. [Bekannte Grenzen](10-known-limits.md)
11. [Betriebshandbuch](11-operator-runbook.md)
12. [Konfigurationsreferenz](12-configuration-reference.md)
13. [Image-Builds und Registries (Entwurf)](13-image-builds.md)
14. [Dienste (Entwurf)](13-services.md)

Lesereihenfolge für einen ersten Test: 1, 2, 3, danach 10, bevor Sie irgendetwas vertrauen.
