import 'dart:async';
import 'dart:math' as math;
import 'dart:typed_data';
import 'dart:ui' as ui;

import 'package:http/http.dart' as http;

import '../app_config.dart';

/// Boat-icon sizing, ported verbatim from the web app
/// (src/marineMap.svelte:143-184).
///
/// The reference vessel is 24.38 m (80 ft) × 6.0 m beam; a dimension scales
/// by `sqrt(value/reference)` clamped to [0.6, 2.5], so a tanker reads
/// bigger without filling the harbour. Length drives the long axis, beam the
/// cross-axis — and when beam is unknown the cross-axis stays at 1, so a
/// long vessel reads long, not also fat.
const double boatImageNaturalWidth = 24;
const double boatImageNaturalHeight = 73;
const double defaultBoatLengthM = 24.38; // 80 ft
const double defaultBoatBeamM = 6.0; // typical 80 ft motoryacht beam
const double boatScaleMin = 0.6;
const double boatScaleMax = 2.5;

/// Floor on rendered width for the override icon: some are tall thin
/// silhouettes and the height-ratio remap leaves them a few pixels wide.
const double myBoatMinRenderedWidthPx = 20;

double dimScaleFactor(double? valueMeters, double referenceMeters) {
  if (valueMeters == null || !valueMeters.isFinite || valueMeters <= 0) {
    return 1;
  }
  final f = math.sqrt(valueMeters / referenceMeters);
  return f.clamp(boatScaleMin, boatScaleMax);
}

/// `(sx, sy)` — X across the boat (beam), Y along it (length). Unknown beam
/// leaves X at 1 so only the long axis grows.
({double sx, double sy}) boatScaleAxes(double? lengthM, double? beamM) => (
      sx: (beamM != null && beamM > 0)
          ? dimScaleFactor(beamM, defaultBoatBeamM)
          : 1,
      sy: dimScaleFactor(lengthM, defaultBoatLengthM),
    );

/// The operator's /myboat-icon override (module config `myboat_icon_path`),
/// probed once per session; the bundled SVG stays on any failure. The
/// override is height-matched to the bundled icon's natural 73 px so it
/// renders at the same on-screen size at any source resolution, with a
/// minimum rendered width so a narrow silhouette isn't a sliver.
class MyBoatIcon {
  static Uint8List? overrideBytes;
  static double? naturalWidth;
  static double? naturalHeight;
  static bool _probed = false;

  static Future<void> probe() async {
    if (_probed) return;
    _probed = true;
    try {
      final r = await http
          .get(Uri.parse('${AppConfig.tileBase.value}/myboat-icon'))
          .timeout(const Duration(seconds: 5));
      if (r.statusCode != 200 || r.bodyBytes.isEmpty) return;
      final image = await decodeImage(r.bodyBytes);
      if (image.width <= 0 || image.height <= 0) return;
      naturalWidth = image.width.toDouble();
      naturalHeight = image.height.toDouble();
      overrideBytes = r.bodyBytes;
      image.dispose();
    } catch (_) {
      // No endpoint / undecodable — keep the bundled default.
    }
  }

  static Future<ui.Image> decodeImage(Uint8List bytes) async {
    final codec = await ui.instantiateImageCodec(bytes);
    final frame = await codec.getNextFrame();
    return frame.image;
  }

  /// Rendered logical-pixel size for the current icon at scale axes
  /// [sx]/[sy]. The bundled SVG is 24×73; an override is remapped by height
  /// ratio, then bumped uniformly if its width would fall under the floor.
  static ({double width, double height}) renderedSize(double sx, double sy) {
    final nh = naturalHeight;
    final nw = naturalWidth;
    if (overrideBytes == null || nh == null || nw == null || nh <= 0) {
      return (
        width: boatImageNaturalWidth * sx,
        height: boatImageNaturalHeight * sy
      );
    }
    final ratio = boatImageNaturalHeight / nh;
    var rsx = sx * ratio;
    var rsy = sy * ratio;
    final renderedWidth = nw * rsx;
    if (renderedWidth < myBoatMinRenderedWidthPx && renderedWidth > 0) {
      final bump = myBoatMinRenderedWidthPx / renderedWidth;
      rsx *= bump;
      rsy *= bump;
    }
    return (width: nw * rsx, height: nh * rsy);
  }
}

/// Square marker-box side that fits the icon at any rotation (its diagonal).
double boatMarkerBoxSide({double sx = 1, double sy = 1}) {
  final s = MyBoatIcon.renderedSize(sx, sy);
  return math.sqrt(s.width * s.width + s.height * s.height);
}
