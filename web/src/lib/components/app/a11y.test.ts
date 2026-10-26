import { describe, expect, it } from 'vitest';
import { render } from 'svelte/server';
import { contrast, tokens } from '../../../../scripts/contrast.mjs';
import StateView from './StateView.svelte';
import FormErrors from './FormErrors.svelte';

describe('accessible shared states', () => {
  it('announces loading and errors with distinct live roles', () => {
    expect(render(StateView, { props: { kind: 'loading', message: 'loading' } }).body).toContain('role="status"');
    const error = render(StateView, { props: { kind: 'error', message: 'apiNotAvailable' } }).body;
    expect(error).toContain('role="alert"');
    expect(error).toContain('API');
  });
  it('announces and lists validation errors', () => {
    const html = render(FormErrors, { props: { errors: ['valSvcName', 'valSvcImage'] } }).body;
    expect(html).toContain('role="alert"');
    expect(html).toContain('<li>');
  });
});

describe('light and dark contrast tokens', () => {
  it('keeps text at 4.5:1 and focus/control borders at 3:1', () => {
    for (const mode of [':root', '.dark']) {
      const t = tokens(mode);
      for (const fg of ['--foreground', '--muted-foreground', '--destructive']) {
        expect(contrast(t[fg], t['--background']), `${mode} ${fg}`).toBeGreaterThanOrEqual(4.5);
      }
      for (const line of ['--ring', '--input', '--border']) {
        expect(contrast(t[line], t['--background']), `${mode} ${line}`).toBeGreaterThanOrEqual(3);
      }
      expect(contrast(t['--primary'], t['--primary-foreground']), `${mode} primary`).toBeGreaterThanOrEqual(4.5);
    }
    for (const accent of ['blue', 'green', 'violet', 'orange']) {
      const light = { ...tokens(':root'), ...tokens(`:root[data-accent='${accent}']`) };
      const dark = { ...tokens('.dark'), ...tokens(`.dark[data-accent='${accent}']`) };
      expect(contrast(light['--primary'], light['--primary-foreground']), `${accent} light primary`).toBeGreaterThanOrEqual(4.5);
      expect(contrast(light['--ring'], light['--background']), `${accent} light ring`).toBeGreaterThanOrEqual(3);
      expect(contrast(dark['--primary'], dark['--primary-foreground']), `${accent} dark primary`).toBeGreaterThanOrEqual(4.5);
    }
  });
});
