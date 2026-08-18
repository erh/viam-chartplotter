import 'package:flutter_test/flutter_test.dart';
import 'package:viam_chartplotter_mobile/tile_sources.dart';

// Committed on purpose: it doubles as a trivial smoke test AND stops
// `flutter create .` (run in CI) from regenerating its broken template test,
// which referenced a nonexistent `package:mobile`.
void main() {
  test('base map layers are defined', () {
    expect(baseLayers, isNotEmpty);
    expect(baseLayers.first.urlTemplate, contains('{z}/{x}/{y}'));
  });
}
