import type { Component } from 'svelte';
import CircleCheck from '@lucide/svelte/icons/circle-check';
import CircleX from '@lucide/svelte/icons/circle-x';
import CircleQuestionMark from '@lucide/svelte/icons/circle-question-mark';
import TriangleAlert from '@lucide/svelte/icons/triangle-alert';
import OctagonAlert from '@lucide/svelte/icons/octagon-alert';
import Info from '@lucide/svelte/icons/info';
import Clock from '@lucide/svelte/icons/clock';
import LoaderCircle from '@lucide/svelte/icons/loader-circle';
import type { OperationState, Severity, State } from '../../api';
import type { MessageKey } from '../../i18n';

export type Tone = 'ok' | 'warn' | 'bad' | 'neutral' | 'unknown';
export interface StatusView {
	icon: Component;
	label: MessageKey;
	tone: Tone;
}

const unknown: StatusView = { icon: CircleQuestionMark, label: 'stateUnknown', tone: 'unknown' };

const stateViews: Record<State, StatusView> = {
	healthy: { icon: CircleCheck, label: 'stateHealthy', tone: 'ok' },
	degraded: { icon: TriangleAlert, label: 'stateDegraded', tone: 'warn' },
	unavailable: { icon: CircleX, label: 'stateUnavailable', tone: 'bad' },
	unknown
};

const operationViews: Record<OperationState, StatusView> = {
	pending: { icon: Clock, label: 'opPending', tone: 'neutral' },
	running: { icon: LoaderCircle, label: 'opRunning', tone: 'neutral' },
	succeeded: { icon: CircleCheck, label: 'opSucceeded', tone: 'ok' },
	failed: { icon: CircleX, label: 'opFailed', tone: 'bad' },
	aborted: { icon: CircleX, label: 'opAborted', tone: 'warn' },
	unknown
};

const severityViews: Record<Severity | 'error', StatusView> = {
	error: { icon: CircleX, label: 'sevError', tone: 'bad' },
	info: { icon: Info, label: 'sevInfo', tone: 'neutral' },
	warning: { icon: TriangleAlert, label: 'sevWarning', tone: 'warn' },
	critical: { icon: OctagonAlert, label: 'sevCritical', tone: 'bad' },
	unknown
};

// Anything outside the known sets falls back to "unknown", never to a healthy look.
export function statusView(kind: 'state' | 'operation' | 'severity', value: string): StatusView {
	const table: Record<string, StatusView> =
		kind === 'state' ? stateViews : kind === 'operation' ? operationViews : severityViews;
	return Object.hasOwn(table, value) ? table[value] : unknown;
}

export const toneClass: Record<Tone, string> = {
	ok: 'text-emerald-700 dark:text-emerald-400',
	warn: 'text-amber-700 dark:text-amber-400',
	bad: 'text-red-700 dark:text-red-400',
	neutral: 'text-sky-700 dark:text-sky-400',
	unknown: 'text-muted-foreground'
};
