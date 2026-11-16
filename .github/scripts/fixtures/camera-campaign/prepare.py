#!/usr/bin/env python3
"""Build a small local HF-compatible model and encoded camera scenes, never a published model."""
import argparse
import hashlib
import io
import json
from pathlib import Path
import uuid

import av
import torch
from PIL import Image
from transformers import DetrConfig, DetrForObjectDetection, DetrImageProcessor, ResNetConfig


def main():
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--runtime-root',type=Path,required=True)
    parser.add_argument('--output',type=Path,required=True)
    args=parser.parse_args()
    root=args.runtime_root.resolve(); output=args.output.resolve()
    output.mkdir(parents=True,exist_ok=False,mode=0o700)
    root.mkdir(parents=True,exist_ok=True,mode=0o700)
    torch.set_num_threads(2);torch.manual_seed(1890)
    decoded=[]; files={}
    # Every packet is a decodable SPS/PPS/IDR access unit. No device is opened.
    for name,color in [('red',(255,0,0)),('blue',(0,0,255))]:
        codec=av.CodecContext.create('libx264','w');codec.width=64;codec.height=64
        codec.pix_fmt='yuv420p';codec.options={'preset':'ultrafast','tune':'zerolatency','x264-params':'keyint=1:repeat-headers=1'}
        packets=codec.encode(av.VideoFrame.from_image(Image.new('RGB',(64,64),color)))+codec.encode(None)
        payload=b''.join(bytes(p) for p in packets)
        with av.open(io.BytesIO(payload),format='h264') as stream:
            frames=list(stream.decode(video=0))
        if len(frames)!=1:raise ValueError('encoder did not produce one independently decoded frame')
        decoded.append(frames[0].to_image())
        path=output/(name+'.h264');path.write_bytes(payload)
        files[path.name]=hashlib.sha256(payload).hexdigest()
    backbone=ResNetConfig(num_channels=3,embedding_size=8,hidden_sizes=[8,16,24,32],depths=[1,1,1,1],layer_type='basic',out_features=['stage4'])
    config=DetrConfig(backbone_config=backbone,use_timm_backbone=False,use_pretrained_backbone=False,d_model=32,num_queries=2,encoder_layers=1,decoder_layers=1,encoder_attention_heads=4,decoder_attention_heads=4,encoder_ffn_dim=64,decoder_ffn_dim=64,num_labels=1,id2label={0:'fixture_red'},label2id={'fixture_red':0})
    model=DetrForObjectDetection(config).eval()
    processor=DetrImageProcessor(size={'shortest_edge':64,'longest_edge':64})
    # Fit only the class head to real decoded scene embeddings. The production
    # worker must still decode pixels and execute this complete DETR forward pass.
    with torch.inference_mode():
        inputs=processor(images=decoded,return_tensors='pt')
        latent=model.model(**inputs).last_hidden_state.reshape(4,32)
        design=torch.cat([latent,torch.ones(4,1)],dim=1)
        target=torch.tensor([[10.,-10.],[10.,-10.],[-10.,10.],[-10.,10.]])
        weights=torch.linalg.lstsq(design.double(),target.double()).solution.float()
        model.class_labels_classifier.weight.copy_(weights[:-1].T)
        model.class_labels_classifier.bias.copy_(weights[-1])
        observed=processor.post_process_object_detection(model(**inputs),threshold=.9,target_sizes=[(64,64)]*2)
        if len(observed[0]['scores'])!=2 or len(observed[1]['scores'])!=0:
            raise ValueError('independent local model reference did not distinguish decoded scenes')
    staging=output/'model';staging.mkdir()
    model.save_pretrained(staging,safe_serialization=True);processor.save_pretrained(staging)
    model_files={p.name:hashlib.sha256(p.read_bytes()).hexdigest() for p in sorted(staging.iterdir())}
    revision=hashlib.sha1(json.dumps(model_files,sort_keys=True).encode()).hexdigest()
    repository='wendy-regression/camera-'+uuid.uuid4().hex
    snapshot=root/'models/hub'/('models--'+repository.replace('/','--'))/'snapshots'/revision
    snapshot.mkdir(parents=True,exist_ok=False)
    for path in staging.iterdir():path.rename(snapshot/path.name)
    staging.rmdir()
    fixture={'schema':1,'runtime_root':str(root),'model':repository,'revision':revision,'model_files':model_files,'files':files,'label':'fixture_red','reference':{'red_detections':2,'blue_detections':0,'red_scores':observed[0]['scores'].tolist()},'scope':'generated local DETR weights and encoded scenes; no published model accuracy or camera/GPU claim'}
    (output/'fixture.json').write_text(json.dumps(fixture,indent=2)+'\n')
    print(json.dumps(fixture,indent=2))


if __name__=='__main__':main()
