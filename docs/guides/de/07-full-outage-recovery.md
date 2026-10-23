---
title: Wiederherstellung nach Totalausfall mit Freigabe
---

# Wiederherstellung nach Totalausfall mit Freigabe

[Übersicht](index.md) | Zurück: [Knotenersatz](06-node-replacement.md) | Weiter: [Sicherheitsmodell](08-security-model.md)

Ein Totalausfall heißt: Kein Knoten ist in einer Primary-Komponente. Der Manager führt **nie** selbstständig einen Bootstrap aus: Er diagnostiziert nur lesend, schlägt einen Kandidaten vor und handelt erst nach ausdrücklicher Freigabe genau des gezeigten Plans.

## Ablauf

1. **Diagnose (nur lesend):** `GET /api/v1/clusters/{id}/outage-diagnosis`. Liest über die Agenten die `grastate.dat` jedes Knotens und verbindet sie mit der letzten Zustandsbeobachtung. Startet nichts; antwortet `409`, solange eine Primary-Komponente existiert. Das Ergebnis zeigt einen vorgeschlagenen Kandidaten oder begründet, warum es keinen gibt (z. B. Gleichstand bei gleichem Seqno nach sauberem Herunterfahren, oder `seqno: -1` auf allen Knoten nach einem Absturz).
2. **Positionen abgestürzter Knoten ermitteln (ändert nur den Crash-Recovery-Zustand):** `POST /api/v1/clusters/{id}/outage-diagnosis` führt zusätzlich `mariadbd --wsrep-recover` in einmaligen Containern ohne Netzwerk für Knoten ohne gespeichertes Seqno aus. Das ist InnoDB-Crash-Recovery auf den gestoppten Daten wie bei jedem Start; es startet keinen Datenbankdienst und führt nie einen Bootstrap aus.
3. **Plan anfordern:** `POST /api/v1/clusters/{id}/outage-recovery` mit `{"candidate": "<Slot>"}`. Der Manager sammelt die Belege erneut und lehnt mit `409` ab, wenn die frische Diagnose keinen oder einen anderen Kandidaten vorschlägt, eine Primary-Komponente existiert oder eine andere Operation unfertig ist. Sonst speichert er den Plan und liefert einen **Freigabe-Digest**. Noch wird nichts gestoppt oder gestartet.
4. **Freigeben:** `POST /api/v1/operations/{id}/approve` mit dem Digest (sonst `409 approval_mismatch`).
5. Der Plan: alle Knoten stoppen, warten bis kein Task läuft, jeden Identitätsmarker prüfen, Position des Kandidaten erneut prüfen und `safe_to_bootstrap` setzen, Kandidaten bootstrappen, übrige Knoten nacheinander starten, Bootstrap-Flag entfernen.

Die Operation braucht die Berechtigung Betreiben auf dem Cluster; die Freigabe nutzt derzeit die Berechtigung **Wiederherstellen** auf den Backups des Clusters (bekannte Unstimmigkeit, siehe [Bekannte Grenzen](10-known-limits.md)).

![Diagnose bei Komplettausfall und geplante Wiederherstellung mit Freigabe](../../assets/screenshots/de-light-outage-recovery.png)

*Bildschirmfoto aus dem Entwicklungs-Mock-Server (`npm run dev:mock`, erfundene Beispieldaten), nicht von einem echten Manager.*

## Wahl des Kandidaten

Folgen Sie der Diagnose. Nennt sie keinen, erzwingen Sie keinen. Bei Verdacht auf Divergenz oder Datenverlust stellen Sie aus einem Backup wieder her ([Backups](05-backup-restore.md)). Der Manager befördert nie automatisch eine Cluster-übergreifende Backup-Kopie.

## Prüfstand

| Teil | Stufe |
| --- | --- |
| Totalausfall nach sauberem Herunterfahren und nach Absturz auf einem 3-Knoten-MariaDB-11.4-Cluster: korrekte lesende Diagnose (Gleichstand; kein Kandidat bis `--wsrep-recover`), 120 s keine Änderung, Wiederherstellung nach Freigabe in 40 s und 61 s, Daten und State-UUID erhalten | [Lab-VMs] (E6, letzter Lauf Exit 0) |
| Eine Wiederherstellung, die still eine neue Historie ab `seqno: -1` begann, wurde gefunden und behoben | [Lab-VMs] (E6, mit Regressionstest) |
| 5-Knoten-Cluster, abweichende Datadir-Layouts, Stromausfall einer VM | [Ungetestet] |
| Ereignis oder Benachrichtigung bei Totalausfall | [Ungetestet]: nicht implementiert |
