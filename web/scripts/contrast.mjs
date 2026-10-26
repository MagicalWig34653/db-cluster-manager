// @ts-nocheck
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';

export function color(token) {
  const match = token.match(/oklch\(([\d.]+)\s+([\d.]+)\s+([\d.]+)\)/);
  if (!match) throw new Error(`Unsupported color: ${token}`);
  const [L, C, H] = match.slice(1).map(Number);
  const a = C * Math.cos(H * Math.PI / 180), b = C * Math.sin(H * Math.PI / 180);
  const l = (L + 0.3963377774 * a + 0.2158037573 * b) ** 3;
  const m = (L - 0.1055613458 * a - 0.0638541728 * b) ** 3;
  const s = (L - 0.0894841775 * a - 1.291485548 * b) ** 3;
  return [
    4.0767416621*l - 3.3077115913*m + 0.2309699292*s,
    -1.2684380046*l + 2.6097574011*m - 0.3413193965*s,
    -0.0041960863*l - 0.7034186147*m + 1.707614701*s
  ].map((v) => Math.max(0, Math.min(1, v)));
}

export function contrast(a, b) {
  const luminance = (rgb) => rgb[0]*0.2126 + rgb[1]*0.7152 + rgb[2]*0.0722;
  const [light, dark] = [luminance(color(a)), luminance(color(b))].sort((x, y) => y - x);
  return (light + 0.05) / (dark + 0.05);
}

export function tokens(selector) {
  const css = readFileSync(fileURLToPath(new URL('../src/app.css', import.meta.url)), 'utf8');
  const block = css.slice(css.indexOf(selector + ' {') + selector.length + 2).split('}')[0];
  if (!css.includes(selector + ' {')) throw new Error(`Missing selector ${selector}`);
  return Object.fromEntries([...block.matchAll(/(--[\w-]+):\s*(oklch\([^;]+\));/g)].map((m) => [m[1], m[2]]));
}
