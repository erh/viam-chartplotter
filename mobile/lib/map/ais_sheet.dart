import 'package:flutter/material.dart';

import '../ais.dart';

/// Bottom sheet with one AIS target's detail — the touch equivalent of the web
/// app's vessel popup (src/marineMap.svelte).
///
/// Task D3 adds CPA/TCPA here; D4 adds the MMSI country flag.
void showAisDetails(BuildContext context, AisBoat b) {
  showModalBottomSheet<void>(
    context: context,
    showDragHandle: true,
    builder: (ctx) => SafeArea(
      child: Padding(
        padding: const EdgeInsets.fromLTRB(20, 0, 20, 20),
        child: Column(
          mainAxisSize: MainAxisSize.min,
          crossAxisAlignment: CrossAxisAlignment.start,
          children: [
            Text(b.displayName, style: Theme.of(ctx).textTheme.titleLarge),
            const SizedBox(height: 2),
            Text('MMSI ${b.mmsi}',
                style: const TextStyle(color: Colors.white54, fontSize: 12)),
            const SizedBox(height: 12),
            _row('SOG', '${b.sogKn.toStringAsFixed(1)} kn'),
            _row('COG',
                b.cogDeg == null ? '—' : '${b.cogDeg!.toStringAsFixed(0)}°'),
            _row(
                'Heading',
                b.headingDeg == null
                    ? '—'
                    : '${b.headingDeg!.toStringAsFixed(0)}°'),
            if (b.lengthM != null)
              _row('Length', '${b.lengthM!.toStringAsFixed(0)} m'),
            if (b.beamM != null)
              _row('Beam', '${b.beamM!.toStringAsFixed(0)} m'),
            if (b.destination != null) _row('Destination', b.destination!),
            _row(
                'Position',
                '${b.location.latitude.toStringAsFixed(5)}, '
                    '${b.location.longitude.toStringAsFixed(5)}'),
          ],
        ),
      ),
    ),
  );
}

Widget _row(String k, String v) => Padding(
      padding: const EdgeInsets.symmetric(vertical: 4),
      child: Row(
        children: [
          SizedBox(
            width: 96,
            child: Text(k,
                style: const TextStyle(color: Colors.white60, fontSize: 13)),
          ),
          Expanded(
            child: Text(v,
                style:
                    const TextStyle(fontSize: 15, fontWeight: FontWeight.w600)),
          ),
        ],
      ),
    );
