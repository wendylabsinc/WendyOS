import {build} from 'esbuild';
import {writeFile} from 'node:fs/promises';
const result=await build({entryPoints:['src/main.tsx'],alias:{three:'../../go/simulator/go2/go2_sim/vendor/three.module.js'},bundle:true,write:false,minify:true,format:'iife',target:'es2022',define:{'process.env.NODE_ENV':'"production"'},legalComments:'eof',loader:{'.css':'text','.svg':'dataurl','.woff2':'dataurl','.webp':'dataurl'}});
const script=result.outputFiles[0].text.replaceAll('</script','<\\/script');
await writeFile('../../go/internal/cli/mcp/desktop_app.html',`<!doctype html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Wendy devices</title></head><body><div id="root"></div><script>${script}</script></body></html>`);
