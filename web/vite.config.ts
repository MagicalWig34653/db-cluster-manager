import adapter from '@sveltejs/adapter-static';
import { sveltekit } from '@sveltejs/kit/vite';
import tailwindcss from '@tailwindcss/vite';
import { defineConfig } from 'vitest/config';

export default defineConfig({
	plugins: [
		tailwindcss(),
		sveltekit({
			// The Go manager embeds this directory (internal/webui/assets).
			adapter: adapter({
				pages: '../internal/webui/assets',
				assets: '../internal/webui/assets',
				fallback: 'index.html',
				precompress: false,
				strict: true
			})
		})
	],
	server: { proxy: { '/api': 'http://127.0.0.1:8080' } },
	test: { include: ['src/**/*.test.ts'] }
});
