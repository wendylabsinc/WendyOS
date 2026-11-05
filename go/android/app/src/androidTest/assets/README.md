# H.264 rendering fixture

`multislice-640x480.h264` contains twelve synthetic frames, red above blue, with
four slices per picture and no access-unit delimiters. Generate it with:

```sh
ffmpeg -f lavfi -i 'color=c=red:s=640x480:r=10,drawbox=x=0:y=240:w=640:h=240:color=blue:t=fill' \
  -frames:v 12 -c:v libx264 -threads 1 -profile:v baseline -pix_fmt yuv420p -tune zerolatency \
  -x264-params 'slices=4:aud=0:keyint=5:repeat-headers=1' -f h264 multislice-640x480.h264
```
