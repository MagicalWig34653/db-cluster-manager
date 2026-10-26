# Pläne für Docker-Engine-Updates

Die Seite **Docker-Engine-Updates** zeigt einen lesenden Plan für jeweils einen
Swarm-Host. Host und Zielversion auswählen; dann Kompatibilität, Sperren,
Galera-Wartungsplan, Schritte und Grenzen einer Rückkehr prüfen. Der Digest des
Plans wird beim Runbook-Export erneut geprüft. Nicht verfügbare oder in der
Kompatibilitätsmatrix ungetestete Zielversionen bleiben gesperrt.

Im ausgelieferten Manager ist derzeit **keine Paketversionsquelle
konfiguriert**. Daher bleibt der Export gesperrt. Der Entwicklungs-Mock zeigt
erfundene Versionen und einen exportierbaren Beispielplan; er belegt weder eine
echte Paketverfügbarkeit noch ein durchgeführtes Update. Eine spätere
Versionsquelle muss zur Installationsmethode des Hosts passen.

Sobald eine passende Versionsquelle konfiguriert ist, gilt diese Reihenfolge:

1. Version, Swarm-Manager-Quorum, betroffene Galera-Knoten und Material für
   eine mögliche Rückkehr prüfen. Für den Plan darf jeweils nur ein Host des
   Swarms in Wartung sein.
2. Unter **Hosts** die geführte Wartung starten und auf **bereit** warten. Der
   Manager stoppt betroffene Galera-Knoten erst nach seinen Sicherheitsprüfungen;
   er drainiert oder startet den Host nicht neu.
3. Update-Plan erneut laden. Bei einem Swarm-Manager-Host einen anderen
   erreichbaren Manager-Endpunkt prüfen und die Warnung zum angepinnten Manager
   ausdrücklich bestätigen. Mit dem aktuellen Digest das Runbook herunterladen.
4. Das Shell-Skript lokal ausführen, um die Anweisungen **anzuzeigen**. Docker
   Drain und die Installation der exakten Version mit der freigegebenen
   Paketmethode des Hosts manuell ausführen. Swarm-Quorum, Docker-Version,
   Node-Status und Galera prüfen, bevor der Node wieder aktiv gesetzt wird.
5. Die geführte Wartung unter **Hosts** beenden und prüfen, dass alle betroffenen
   Galera-Knoten Synced sind. Einen fehlerhaften Node drainiert lassen und bei
   Bedarf die geführte Recovery verwenden; Galera nicht manuell bootstrappen.

Der Manager führt Upgrade, Drain, Neustart und Rollback niemals selbst aus.
Ein Paket-Downgrade ist möglicherweise nicht unterstützt. Änderungen an
Inventar oder Wartung machen den bisherigen Digest ungültig. Die Grenzen für
Engine-Updates bleiben eine Produktentscheidung in
[ADR 0019](../../decisions/0019-docker-engine-update-plans.md).
