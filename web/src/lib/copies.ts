import type { BackupCopy } from './api';

/** Copies a restore may start from: succeeded copies of the cluster, newest first. */
export function copyChoices(copies: BackupCopy[], cluster: string): BackupCopy[] {
	return copies
		.filter((c) => c.cluster === cluster && c.status === 'succeeded')
		.sort((a, b) => Date.parse(b.finishedAt ?? b.startedAt) - Date.parse(a.finishedAt ?? a.startedAt));
}
