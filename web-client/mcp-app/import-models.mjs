// Import existing Wendy website display assets, decoding Draco once at build
// time so the sandbox needs neither a CDN, a WASM decoder, nor worker scripts.
import {NodeIO} from '@gltf-transform/core';
import {ALL_EXTENSIONS} from '@gltf-transform/extensions';
import draco from 'draco3dgltf';
import {weld,simplify,quantize} from '@gltf-transform/functions';
import {MeshoptSimplifier} from 'meshoptimizer';
import {mkdir,readFile,writeFile} from 'node:fs/promises';
import {createHash} from 'node:crypto';
import path from 'node:path';
const root=process.argv[2];if(!root)throw Error('Pass the Wendy marketing-website checkout');
const target=new URL('../../go/internal/cli/mcp/desktop_assets/',import.meta.url);
await mkdir(target,{recursive:true});
const io=new NodeIO().registerExtensions(ALL_EXTENSIONS).registerDependencies({'draco3d.decoder':await draco.createDecoderModule()});
const models={thor:'jetson-thor/nvidia-jetson-thor-devkit-web.glb',go2:'unitree-go2/go2.web.glb',orin:'quickstart/jetson-orin-nano.glb',dragonwing:'quickstart/dragonwing-iq-9075.glb',dgx:'dgx-spark/nvidia-dgx-spark.glb',macbook:'macbook/macbook.web.glb'};
const manifest={};
for(const [id,file]of Object.entries(models)){
 const source=path.join(root,'public/models',file),doc=await io.read(source);
 for(const extension of doc.getRoot().listExtensionsUsed())if(extension.extensionName==='KHR_draco_mesh_compression')extension.dispose();
 await MeshoptSimplifier.ready;
 await doc.transform(weld(),simplify({simplifier:MeshoptSimplifier,ratio:id==='go2'?0.8:0.12,error:0.002}),quantize());
 const bytes=await io.writeBinary(doc);await writeFile(new URL(id+'.glb',target),bytes);
 manifest[id]={source:'marketing-website/public/models/'+file,sourceSHA256:createHash('sha256').update(await readFile(source)).digest('hex'),sha256:createHash('sha256').update(bytes).digest('hex'),bytes:bytes.length};
}
await writeFile(new URL('manifest.json',target),JSON.stringify(manifest,null,2)+'\n');
await writeFile(new URL('GO2-LICENSE.md',target),await readFile(path.join(root,'public/models/unitree-go2/README.md')));
