import vue from '@vitejs/plugin-vue'
import vueJsx from '@vitejs/plugin-vue-jsx'
import AutoImport from 'unplugin-auto-import/vite'
import Icons from 'unplugin-icons/vite'
import Components from 'unplugin-vue-components/vite'
import UnoCSS from 'unocss/vite'
import compression from 'vite-plugin-compression'
import vueDevtools from 'vite-plugin-vue-devtools'

export function createVitePlugins(env) {
  const plugins = [
    vue(),
    vueJsx(),
    UnoCSS(),
    AutoImport({
      imports: ['vue', 'vue-router', 'vue-i18n', '@vueuse/core'],
      dts: 'src/auto-imports.d.ts',
    }),
    Components({
      dts: 'src/components.d.ts',
    }),
    Icons(),
  ]

  if (env.VITE_GZIP === 'true') {
    plugins.push(compression({ algorithm: 'gzip' }))
  }

  if (env.VITE_DEVTOOLS === 'true') {
    plugins.push(vueDevtools())
  }

  return plugins
}
