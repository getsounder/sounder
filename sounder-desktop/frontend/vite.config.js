import { defineConfig } from "vite";

// 前端通过 query 参数拿本地 Worker 地址，不依赖 Wails 绑定，
// 因此不需要 @wailsio/runtime 的 typed-events 插件。
export default defineConfig({
  plugins: [],
});
