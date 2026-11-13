"""Fixed, restricted CPU checkpoint metadata inspection; never launches training."""
import hashlib
import io
import json
import os
from pathlib import Path
import stat
import sys
import zipfile

MAX_CHECKPOINT = 64 * 1024 * 1024


def digest(path, limit):
    fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    with os.fdopen(fd, 'rb') as stream:
        info = os.fstat(stream.fileno())
        if not stat.S_ISREG(info.st_mode) or info.st_size > limit:
            raise ValueError('artifact must be a bounded regular file')
        result = hashlib.sha256()
        remaining = limit + 1
        while remaining:
            chunk = stream.read(min(1024 * 1024, remaining))
            if not chunk: break
            result.update(chunk)
            remaining -= len(chunk)
        if remaining == 0: raise ValueError('artifact grew beyond inspection limit')
        return result.hexdigest(), info


def inspect(request):
    import numpy as np
    import torch
    path = Path(request['path'])
    fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    with os.fdopen(fd, 'rb') as stream:
        before = os.fstat(stream.fileno())
        if not stat.S_ISREG(before.st_mode) or before.st_size > MAX_CHECKPOINT:
            raise ValueError('artifact must be a bounded regular file')
        raw = stream.read(MAX_CHECKPOINT + 1)
    if len(raw) > MAX_CHECKPOINT: raise ValueError('artifact grew beyond inspection limit')
    expected = hashlib.sha256(raw).hexdigest()
    if expected != request['sha256'].lower(): raise ValueError('checkpoint hash mismatch; no checkpoint loaded')
    # Load only the exact immutable bytes whose hash was checked. Path changes
    # during deserialization cannot substitute a different artifact.
    with zipfile.ZipFile(io.BytesIO(raw)) as archive:
        members = archive.infolist()
        if len(members) > 4096 or sum(x.file_size for x in members) > 256*1024*1024:
            raise ValueError('checkpoint archive exceeds inspection budget')
    with torch.serialization.safe_globals([np.ndarray, np.dtype, np._core.multiarray._reconstruct, np.dtypes.UInt32DType]):
        saved = torch.load(io.BytesIO(raw), map_location='cpu', weights_only=True)
    if not isinstance(saved, dict): raise ValueError('checkpoint must contain a dictionary')
    manifest = saved.get('manifest', {})
    if not isinstance(manifest, dict): manifest = {}
    experiment = manifest.get('experiment', {})
    if not isinstance(experiment, dict): experiment = {}
    scalar = lambda value: value if type(value) in (int, float, bool) or value is None else str(value)[:160]
    result = {'checkpoint_sha256': expected, 'bytes': len(raw), 'restricted_loader': True,
              'training_started': False, 'cuda_execution_verified': False,
              'iteration': scalar(saved.get('iteration')), 'optimizer_steps_total': scalar(saved.get('optimizer_steps_total')),
              'transitions': scalar(saved.get('transitions')),
              'available_state': {k: k in saved for k in ['policy','optimizer','torch_rng','cuda_rng','numpy_rng','python_rng','experiment_rng']},
              'experiment_rng_keys': sorted(str(k)[:80] for k in saved['experiment_rng'])[:32] if isinstance(saved.get('experiment_rng'),dict) else None,
              'experiment': {k:scalar(experiment.get(k)) for k in ['seed','planned_iterations','num_envs','coefficient','actor_scope','experiment_script_sha256']},
              'runtime': {'python':sys.version.split()[0], 'torch':torch.__version__, 'numpy':np.__version__}}
    optimizer = saved.get('optimizer', {})
    if isinstance(optimizer,dict) and isinstance(optimizer.get('state'),dict):
        steps = sorted({float(s['step']) for s in optimizer['state'].values() if isinstance(s,dict) and 'step' in s})
        result['optimizer_parameter_steps'] = steps[:16]
        result['optimizer_parameter_steps_omitted'] = max(0,len(steps)-16)
    project = request.get('source_directory')
    source_hashes = experiment.get('source_hashes')
    if project and isinstance(source_hashes,dict) and 0 < len(source_hashes) <= 256:
        root=Path(project).resolve(strict=True)
        changed=[];missing=[];directories={};checked=0
        for name,wanted in source_hashes.items():
            relative=Path(name)
            if relative.is_absolute() or '..' in relative.parts or len(name)>1024:raise ValueError('checkpoint contains unsafe source paths')
            source=root/relative
            if not source.resolve().is_relative_to(root):raise ValueError('checkpoint source escapes project')
            try:
                actual,_=digest(source,MAX_CHECKPOINT)
                checked+=1
                if not isinstance(wanted,str) or actual!=wanted.lower():changed.append(name)
            except FileNotFoundError:missing.append(name)
            if relative.suffix=='.py' and relative.parent!=Path('.'):
                directories.setdefault(str(relative.parent),set()).add(relative.name)
        extras=[]
        for directory,declared in directories.items():
            folder=root/directory
            if folder.is_symlink() or not folder.resolve().is_relative_to(root):raise ValueError('source directory escapes project')
            for index,p in enumerate(folder.iterdir()):
                if index>=1024:raise ValueError('source directory scan exceeds limit')
                if p.suffix=='.py' and p.name not in declared:extras.append(str(p.relative_to(root)))
        result['source_check']={'declared_files':len(source_hashes),'read_files':checked,'changed':changed[:16],'missing':missing[:16],
                                'extra_python_files':extras[:16],'differences_omitted':max(0,len(changed)-16)+max(0,len(missing)-16)+max(0,len(extras)-16),
                                'declared_hashes_match':not changed and not missing,
                                'python_directory_inventory_matches':not extras and not changed and not missing,
                                'scope':'declared files plus Python siblings in their subdirectories; does not prove a compatible launch'}
        adapter=manifest.get('perturbation_module_sha256')
        if isinstance(adapter,str):
            candidates=[]
            scripts=root/'scripts'
            if scripts.is_dir() and not scripts.is_symlink():
                for index,p in enumerate(scripts.iterdir()):
                    if index>=256:raise ValueError('adapter scan exceeds limit')
                    if p.suffix=='.py' and p.is_file() and not p.is_symlink() and digest(p,1024*1024)[0]==adapter:
                        candidates.append(str(p.relative_to(root)))
            profile=manifest.get('stress_profile')
            encoded=json.dumps(profile,allow_nan=False)
            result['environment_adapter']={'sha256':adapter,'matching_project_files':candidates[:8],
                                           'stress_profile':profile if 'stress_profile' in manifest and len(encoded)<=1024 else None,
                                           'stress_profile_available':'stress_profile' in manifest and len(encoded)<=1024,
                                           'preserve_on_resume':True}
    return result


if __name__ == '__main__':
    try:
        result=inspect(json.loads(sys.argv[1]))
        encoded=json.dumps(result,separators=(',',':'),allow_nan=False)
        if len(encoded.encode())>12000:raise ValueError('checkpoint metadata exceeds response budget')
        print('WENDY_JOB '+encoded,flush=True)
    except Exception as exc:
        print('WENDY_JOB '+json.dumps({'error':str(exc)[:800],'status':'unknown','training_started':False}),flush=True)
        sys.exit(1)
