import { tick } from 'svelte';

/** Move focus after validation has updated aria-invalid in the rendered form. */
export async function focusFirstInvalid(container: HTMLElement | null): Promise<void> {
	await tick();
	container?.querySelector<HTMLElement>('[aria-invalid="true"], [data-invalid="true"]')?.focus();
}
