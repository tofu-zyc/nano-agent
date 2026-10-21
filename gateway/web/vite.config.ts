import { defineConfig } from 'vite';
import { svelte } from '@sveltejs/vite-plugin-svelte';

// 构建结果直接放进网关的 Go 包里，用 go:embed 打进二进制
export default defineConfig({
  plugins: [svelte()],
  build: {
    outDir: '../internal/web/dist',
    emptyOutDir: true,
    assetsInlineLimit: 0,
    chunkSizeWarningLimit: 600,
  },
  server: {
    // 开发时把接口转给本机跑着的网关
    proxy: { '/api': { target: 'http://127.0.0.1:7788', changeOrigin: false } },
  },
});
