import 'package:flutter_test/flutter_test.dart';
import 'package:viam_chartplotter_mobile/screens/machine_picker_screen.dart';

// The picker's per-page search: a pure name filter over whatever list is
// showing (orgs, locations, or machines).

class _Named {
  _Named(this.name);
  final String name;
}

void main() {
  final items = [
    _Named('Checkmate'),
    _Named('Eliot16'),
    _Named('Andiamo'),
  ];

  test('filters by name, case-insensitively', () {
    expect(filterByName(items, 'checkm'), hasLength(1));
    expect((filterByName(items, 'ELIOT').single as _Named).name, 'Eliot16');
  });

  test('a blank query filters nothing', () {
    expect(filterByName(items, ''), hasLength(3));
    expect(filterByName(items, '   '), hasLength(3));
  });

  test('no matches yields an empty list', () {
    expect(filterByName(items, 'zzz'), isEmpty);
  });
}
