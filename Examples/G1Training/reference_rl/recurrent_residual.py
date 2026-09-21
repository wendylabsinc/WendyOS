"""Reference-conditioned PPO with causal, per-world visual/joint memory."""
import torch
from torch import nn
from policy import ResidualPolicy

ARCHITECTURE = 'reference-residual-gru-v1'


class RecurrentResidualPolicy(ResidualPolicy):
    def __init__(self, normalization):
        super().__init__(normalization)
        self.memory = nn.GRU(256, 128, batch_first=True)
        self.memory_projection = nn.Linear(128, 256)
        # Preserve the parent's correction function exactly at migration.
        # PPO learns to incorporate the transferred BC memory, then adapts it.
        self.memory_gate = nn.Parameter(torch.zeros(()))

    def initial_hidden(self, worlds):
        return self.log_std.new_zeros(1, worlds, 128)

    def sequence(self, features, hidden):
        """Ordered B,T chunks; the caller splits chunks at episode boundaries."""
        memory, next_hidden = self.memory(features[..., :256], hidden)
        sensors = features[..., :256] + self.memory_gate.tanh() * self.memory_projection(memory)
        fused = torch.cat((sensors, features[..., 256:]), -1)
        dist, value = super().forward(fused)
        return dist, value, next_hidden

    def step(self, features, hidden, reset=None):
        if reset is not None:
            hidden = hidden * (~reset.bool())[None, :, None]
        dist, value, hidden = self.sequence(features[:, None], hidden)
        return torch.distributions.Normal(dist.loc[:, 0], dist.scale[:, 0]), value[:, 0], hidden

    def forward(self, features):
        raise RuntimeError('Recurrent policy requires step/sequence and explicit hidden state')


def initialize_recurrent(payload, bc_payload=None, device='cuda'):
    model = RecurrentResidualPolicy(payload['contract']['normalization'])
    if payload['contract'].get('architecture') == ARCHITECTURE:
        model.load_state_dict(payload['model'])
    else:
        if bc_payload is None:
            raise ValueError('Migration requires the original recurrent BC checkpoint')
        missing, unexpected = model.load_state_dict(payload['model'], strict=False)
        expected = {'memory_gate'} | {k for k in model.state_dict() if k.startswith(('memory.', 'memory_projection.'))}
        if set(missing) != expected or unexpected:
            raise ValueError('Unexpected parent architecture during recurrent migration')
        for name in ('memory', 'memory_projection'):
            state = {k[len(name)+1:]: v for k, v in bc_payload['model'].items() if k.startswith(name+'.')}
            getattr(model, name).load_state_dict(state)
        # Features must have exactly the same encoder lineage as BC memory.
        for key, value in model.encoder.state_dict().items():
            if not torch.equal(value, bc_payload['model']['base.'+key]):
                raise ValueError('BC memory and residual encoder lineage differ: '+key)
    return model.to(device)


def ordered_chunks(steps, episode_starts, length):
    boundaries = sorted(set([0, steps] + list(episode_starts)))
    for left, right in zip(boundaries, boundaries[1:]):
        for start in range(left, right, length):
            yield start, min(start+length, right)
