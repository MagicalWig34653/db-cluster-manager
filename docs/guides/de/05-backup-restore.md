---
title: Backups und Wiederherstellung
---

# Backups und Wiederherstellung

[Übersicht](index.md) | Zurück: [Zustand und Alarme](04-health-alerts.md) | Weiter: [Knotenersatz](06-node-replacement.md)

## Begriffe

- **Cluster-Backup:** ein physisches Backup (`mariabackup`), verschlüsselt gestreamt auf ein S3- oder SFTP-Ziel durch einen einmaligen Swarm-Job-Dienst (Job-Image `deploy/Dockerfile.jobs`, `mariadb:11.4.13` plus `dbcm-job`). Ein logischer Export je Datenbank existiert zusätzlich in der Engine.
- **Backup der Manager-Konfiguration** ist ein separates Bibliotheksmerkmal (siehe unten).
- **Inspektions-Wiederherstellung:** Das Backup wird in eine isolierte Einzelknoten-Instanz zurückgespielt (`--wsrep-on=OFF`, eigenes Netzwerk, eigener Port, keine Replikation, kein Binlog), damit Sie die Daten ansehen können. Sie tritt nie dem Produktivcluster bei.

## Voraussetzungen

Ziel-Einstellungen (`manager.backup.backendConfigFile`), ein Recovery-Key-Swarm-Secret, ein Secret mit der Backup-Optionsdatei, das Job-Image auf den betroffenen Knoten und ein Inspektionsnetzwerk. Das Labor nutzte SFTP mit eigenem Benutzer und festgelegtem Host-Key. Das `dir`-Ziel funktioniert mit Jobs nicht (der Job schreibt in seinen eigenen Container).

## Backup starten und Zeitplan setzen

- Sofort: `POST /api/v1/clusters/{id}/backups` (Berechtigung Backup auf dem Cluster). Der Manager wählt einen geeigneten Quellknoten und legt einen eigenen, an diesen Host gebundenen Job an.
- Zeitplan: `PUT /api/v1/clusters/{id}/backup-policy` mit `enabled`, einem fünfstelligen Cron-ähnlichen `schedule`, `timezone`, `missed` (`skip` oder `run-once`) und `method: physical`. `GET /api/v1/backups` liefert `lastBackupAt`, `nextRunAt`, `lastVerifiedRestoreAt`; fehlend heißt unbekannt, nicht „keines“.

## Wiederherstellung per Inspektion mit Freigabe

1. `POST /api/v1/restores/inspect` mit `cluster`, optional `backup` (Standard: neuestes abgeschlossenes), `host`, `keySecret`, `ttlMinutes` (höchstens 1440). Die Antwort enthält den **Freigabe-Digest** und, nur einmal, das Passwort des Inspektionskontos.
2. Plan prüfen, dann `POST /api/v1/operations/{id}/approve` mit dem Digest. Ein falscher Digest ergibt `409 approval_mismatch`. Die Freigabe erfordert die Berechtigung **Wiederherstellen** (Administratoren; nicht im Operator-Preset).
3. Der Job stellt wieder her, die Instanz startet, Sie verbinden sich mit dem einmaligen Konto `dbcm_inspect`. Bei Ablauf werden Instanz, Volume und Init-Secret entfernt.

## Prüfstufen

| Teil | Stufe |
| --- | --- |
| Physisches Backup nach SFTP mit Quelle auf einem Worker (etwa 15 s, 55,7 MB Stream, Manifest mit Checkpoints) | [Lab-VMs] (E4) |
| Inspektions-Wiederherstellung mit Freigabe-Digest, falscher Digest `409`, wiederhergestellte Zeilen lesbar, `wsrep_on 0` | [Lab-VMs] (E4, auf dem Host des Manager-Knotens) |
| Restore- und Inspektionsjobs auf Worker-Hosts (Job über den Agenten des Manager-Knotens, Prüfung des Zielhosts) | [Fakes] |
| Verifikationstor, Bericht auf Instanzebene, `lastVerifiedRestoreAt`, Abbau mit Secret-Entfernung | [Fakes]; Tabelleninhalte werden auch im Entwurf nicht verglichen |
| Galera-Position im Manifest aus dem Stream gelesen | [Fakes]; unbestätigte Annahme für MariaDB 11.4 |
| Backup-Zeitplaner, logisches Backup, S3-Ziel, Wiederherstellung in einen **neuen Cluster**, selektive Wiederherstellung, PITR | [Fakes] oder [Ungetestet] (PITR, selektive Wiederherstellung) |
| Vollständige Wiederherstellung produktionsgroßer Daten, Wiederherstellungsdauer | [Ungetestet] |

Ein Backup gilt erst als gut, wenn eine Wiederherstellung getestet wurde. Bis zu einer geprüften Wiederherstellung Ihrer Daten behandeln Sie das Backup als ungeprüft. Das Manifest enthält Klartext-Chunk-Digests (offene Sicherheitsfrage, ADR 0009).

## Backup der Manager-Konfiguration

`internal/backup` kann verschlüsselte Backups der Manager-Konfiguration auf lokale, S3- oder SFTP-Ziele schreiben und im Inspektionsmodus wiederherstellen. Nichts plant es ein, und weder UI noch API bieten es an [Fakes]. Was stattdessen zu tun ist, steht im [Betriebshandbuch](11-operator-runbook.md).
