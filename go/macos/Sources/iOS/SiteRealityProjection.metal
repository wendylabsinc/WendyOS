#include <metal_stdlib>
#include <RealityKit/RealityKit.h>
using namespace metal;

// RGBA8 stores the exact IEEE-754 bytes. RealityKit exposes custom textures as
// half, so reading raw floating-point depth through that slot loses precision.
float4 wendyPackFloat(float value) {
    uint bits = as_type<uint>(value);
    return float4(bits & 255, (bits >> 8) & 255, (bits >> 16) & 255, bits >> 24) / 255.0;
}
float wendyUnpackFloat(half4 value) {
    uint4 bytes = uint4(round(float4(value) * 255.0));
    return as_type<float>(bytes.x | (bytes.y << 8) | (bytes.z << 16) | (bytes.w << 24));
}
float wendyParameter(texture2d<half> atlas, uint index) { return wendyUnpackFloat(atlas.read(uint2(index, 0))); }
float4 wendyParameter4(texture2d<half> atlas, uint index) {
    return float4(wendyParameter(atlas,index), wendyParameter(atlas,index+1), wendyParameter(atlas,index+2), wendyParameter(atlas,index+3));
}
struct WendyDepthUniforms { float4x4 worldToOptical; float4 extentNearFar; };
struct WendyDepthVertex { float4 position [[position]]; float depth; };
vertex WendyDepthVertex wendyRealityDepthVertex(uint index [[vertex_id]],
    const device float4 *vertices [[buffer(0)]], constant WendyDepthUniforms &u [[buffer(1)]]) {
    float3 p = (u.worldToOptical * vertices[index]).xyz;
    float near = u.extentNearFar.z, far = u.extentNearFar.w;
    return {float4(p.x / u.extentNearFar.x, -p.y / u.extentNearFar.y,
        far / (far-near) * p.z - far*near / (far-near), p.z), p.z};
}
fragment float4 wendyRealityDepthFragment(WendyDepthVertex v [[stage_in]]) { return wendyPackFloat(v.depth); }

bool wendyVisible(texture2d<half> atlas, float3 optical, float3 planeNormal) {
    float2 extent = wendyParameter4(atlas,20).zw;
    if (optical.z <= 0.01) return false;
    float2 uv = 0.5 + 0.5 * optical.xy / optical.z / extent;
    if (any(uv < 0.0) || any(uv >= 1.0)) return false;
    uint2 size(atlas.get_width(), atlas.get_height()-1);
    uint2 pixel = uint2(uv * float2(size));
    float visible = wendyUnpackFloat(atlas.read(pixel + uint2(0,1)));
    float3 ray = float3(((float2(pixel)+0.5)/float2(size)-0.5)*2.0*extent,1);
    float denominator = dot(planeNormal,ray);
    float expected = abs(denominator)>1e-12 ? dot(planeNormal,optical)/denominator : optical.z;
    float tolerance = max(0.003,min(0.02,expected*0.002));
    return visible>0 && expected>0.01 && abs(expected-visible)<=tolerance;
}

[[visible]] void wendyRealityProjection(realitykit::surface_parameters params) {
    auto surface = params.surface();
    constexpr sampler linearSampler(coord::normalized, address::clamp_to_edge, filter::linear);
    float2 baseUV = params.geometry().uv0(); baseUV.y = 1.0-baseUV.y;
    half3 base = params.textures().base_color().sample(linearSampler,baseUV).rgb * half3(params.material_constants().base_color_tint());
    surface.set_base_color(base);
    surface.set_roughness(params.material_constants().roughness_scale());
    surface.set_metallic(params.material_constants().metallic_scale());
    auto atlas = params.textures().custom();
    float4x4 worldToOptical(wendyParameter4(atlas,0),wendyParameter4(atlas,4),wendyParameter4(atlas,8),wendyParameter4(atlas,12));
    float3 optical = (worldToOptical * float4(params.geometry().world_position(),1)).xyz;
    float3 normal = cross(dfdx(optical),dfdy(optical));
    if (!wendyVisible(atlas,optical,normal)) return;
    float2 p=optical.xy/optical.z;
    float4 distortion=wendyParameter4(atlas,24);
    float r2=dot(p,p), radial=1+r2*(distortion.x+r2*(distortion.y+r2*wendyParameter(atlas,28)));
    float2 distorted=p*radial+float2(2*distortion.z*p.x*p.y+distortion.w*(r2+2*p.x*p.x),
        distortion.z*(r2+2*p.y*p.y)+2*distortion.w*p.x*p.y);
    float4 lens=wendyParameter4(atlas,16);
    float2 uv=(distorted*lens.xy+lens.zw)/wendyParameter4(atlas,20).xy;
    if (any(uv<0) || any(uv>1)) return;
    // Emissive output displays the camera's captured lighting without relighting it.
    surface.set_base_color(half3(0));
    surface.set_emissive_color(params.textures().emissive_color().sample(linearSampler,uv).rgb);
}

// The GPU test uses the same visibility function as the actual surface shader.
kernel void wendyRealityVisibilityTest(texture2d<half> atlas [[texture(0)]],
    const device float4 *points [[buffer(0)]], device uint *visible [[buffer(1)]], uint id [[thread_position_in_grid]]) {
    visible[id] = wendyVisible(atlas,points[id].xyz,float3(0,0,1)) ? 1 : 0;
}
