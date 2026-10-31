# E18 part 1: agent port 7443 "connection refused" at backup start (2026-10-31)

Question from E16: cluster events showed "node n1/n2 became unknown ... connection refused" for a few seconds when a backup, archive or inspection started. Hypotheses: (1) host load makes the 5 s health probe fail, (2) the agent container restarts (OOM, Swarm update).

## Setup

`tests/p0/e18-agent-blips.sh`: three fresh `dbcm-lab-` Lima VMs (arm64, 2 vCPU / 4 GiB / 20 GiB), real manager, three mTLS agents, 3-node Galera `e18db` created through the API, SFTP backup target on m1 (as E4). Four sequential "backup now" operations (30 s apart) through the API while every VM recorded: a 5 Hz TCP probe of `127.0.0.1:7443` (state changes with millisecond timestamps), `docker events` for container die/start/kill/oom/stop/destroy, `docker stats` of the agent container every 2 s, `vmstat`, the agent service task list before and after, the kernel log, the agent container log. Host load during the run: 12-14 (1 minute), i.e. a loaded host, comparable to E16.

## Result

| Observation | Value |
| --- | --- |
| Backups | 4 of 4 succeeded, each in 4-5 s |
| Probe of port 7443 on m1, m2, m3 | one line each: `open` at the start, never `closed` during the whole window (about 3 minutes) |
| Agent service tasks before/after | same three tasks, `Running`, no restart, no task history beyond the first task |
| Docker events for the agent container | none (m1, m3); m2 shows only the short-lived job containers |
| Agent memory / CPU | about 10 MiB, 0.1-0.5 % CPU, no spike at job start |
| Kernel OOM kills | none |
| Cluster events "became unknown / healthy again" during the backups | none. The only transition events in the run (`n2`, `n3` "healthy again" at 09:42:19 and 09:42:49) belong to the cluster creation, 90 s before the first backup |

## Conclusion

The blip was **not reproduced**. At a backup start the agent is not restarted, not OOM-killed, does not refuse connections and does not change the Swarm service: hypothesis (2) is refuted for the backup path, and the job start did not make the 5 s probe fail even with a host load of 12-14 (hypothesis (1) not reproduced). The "became unknown" events seen in E16 coincided with the first minutes after the agents were deployed and the cluster was created (this run shows the same "healthy again" events at that time: agent TLS listener and database not yet up when the first probe ran). In E16 the walkthrough also created the cluster, ran backup, archive and inspection within a few minutes of the agent deployment, and one `dbcm-agent` replica showed 1/3 during cluster creation: the most consistent explanation is the settling of freshly deployed agents (certificate enrollment restarts the listener), not the jobs. This is an explanation consistent with the data, **not proven** for E16, whose agents were not instrumented.

No product change: a hysteresis would hide real outages (E4 requires an event after a 30-40 s outage) and no cause in the job path exists. If the symptom returns, run `tests/p0/e18-agent-blips.sh` with `E18_BACKUPS` raised and archive/inspection added; the samplers (`e18-samples/`) separate a restart from a probe timeout.

## Limits

One run, one host load level (12-14, not the 30+ seen at times), arm64 only, backup only (archive and inspection not repeated here), manual backups on idle data. Raw: `e18-agent-blips.log`, `e18-samples/`.
