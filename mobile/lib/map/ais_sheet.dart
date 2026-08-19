import 'package:flutter/material.dart';

import '../ais.dart';
import '../boat_state.dart';
import '../cpa.dart';

/// Bottom sheet with one AIS target's detail — the touch equivalent of the web
/// app's vessel popup (src/marineMap.svelte).
///
/// Task D4 adds the MMSI country flag.
void showAisDetails(BuildContext context, AisBoat b, {BoatState? own}) {
  // CPA/TCPA (D3): only when both vessels have COG and the target hasn't
  // already passed its closest point (web shows it only for tcpa >= 0).
  ({double cpaNm, double tcpaMin})? cpa;
  final ownPos = own?.position;
  if (ownPos != null) {
    cpa = computeCpa(
      ownLat: ownPos.latitude,
      ownLng: ownPos.longitude,
      ownCogDeg: own!.cogDeg,
      ownSpdKn: own.speedKn ?? 0,
      tgtLat: b.location.latitude,
      tgtLng: b.location.longitude,
      tgtCogDeg: b.cogDeg,
      tgtSpdKn: b.sogKn,
    );
    if (cpa != null && cpa.tcpaMin < 0) cpa = null;
  }
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
            if (cpa != null)
              _row(
                  'CPA',
                  '${cpa.cpaNm.toStringAsFixed(2)} nm '
                      'in ${cpa.tcpaMin.toStringAsFixed(0)} min'),
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
