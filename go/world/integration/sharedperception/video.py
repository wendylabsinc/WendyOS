"""Record the combined live native walkthrough with its original capture timing."""
import argparse
from pathlib import Path

from integration.livetwin.video import caption as hazard_caption, encode


def caption(receipt, restored):
    switches = {row['id']: row['enabled'] for row in receipt.get('sensor_sources', [])}
    if switches.get('camera-b') is False:
        return ('Pause one hazard camera', 'The other sensor inputs remain enabled.')
    layers = {row['name']: row['enabled'] for row in receipt.get('layers', [])}
    if layers.get('Tracks') is False:
        return ('Toggle the track overlay', 'Choose which estimates appear in the twin.')
    if layers.get('Observations') is False:
        return ('Focus on the shared estimates', 'The observation overlay is hidden.')
    tracks = [row for row in receipt.get('native_tracks', [])
              if row.get('visible') is True and row.get('fresh_at_start') is True
              and row.get('fresh_at_end') is True]
    if tracks and receipt.get('inspector_panel') == 'People':
        return ('Inspect anonymous person tracks', 'Pose inference uses rendered camera images and calibration.')
    if tracks and receipt.get('current_native_hazard_captured') is True:
        return ('People and hazards in one twin', 'Independent fusion apps share results across two gateways.')
    if tracks:
        return ('Current person tracks', 'Each estimate retains its own source evidence and expiry.')
    return hazard_caption(receipt, restored)


def main():
    from integration.sharedperception import proof
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--run-dir', required=True, type=Path)
    parser.add_argument('--out', required=True, type=Path)
    args = parser.parse_args()
    print(encode(args.run_dir.resolve(strict=True), args.out, proof_module=proof,
                 captioner=caption, subtitle='People + hazard fusion'))


if __name__ == '__main__':
    main()
