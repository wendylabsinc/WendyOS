#!/usr/bin/env python3
"""Combined ordinary apps on the existing disposable two-Agent lifecycle."""
import argparse
from pathlib import Path

from integration.remotemediafusion import libraries
from integration.sharedperception import model, stage

control = libraries.load('sharedperception_control', libraries.HERE / 'control.py',
                         {'libraries': libraries, 'model': model})
control.VOLUME_DIRECTORIES = control.VOLUME_DIRECTORIES.replace('world-remotemediafusion-', 'world-sharedperception-')
base = libraries.load('sharedperception_remote_run', libraries.HERE / 'run.py',
                     {'libraries': libraries, 'model': model, 'stage': stage, 'control': control})


class Runner(base.Runner):
    def service_config(self, node, service, task):
        if service == 'people':
            model.require(node == 'a', 'people belongs to A')
            return model.people_config(self.pair, self.inputs['run_id'], self.scene,
                                       self.inputs['model'], task['generation'])
        if service == 'people-reader':
            model.require(node == 'b', 'people reader belongs to B')
            return model.people_reader_config(self.pair, self.inputs['run_id'], self.scene, task['generation'])
        return super().service_config(node, service, task)

    def observe(self):
        from integration.sharedperception import scenario
        return scenario.execute(self)


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--stage', type=Path, required=True)
    parser.add_argument('--output', type=Path, required=True)
    parser.add_argument('--trace-rpc', action='store_true')
    args = parser.parse_args()
    Runner(args.stage, args.output, trace_rpc=args.trace_rpc).execute()
