import 'package:flutter_test/flutter_test.dart';
import 'package:viam_chartplotter_mobile/screens/machine_picker_screen.dart';

// Boat search on the org screen: the pure filter — match on machine,
// location, or org name; dedupe by machine id; sort by machine name.

class _Named {
  _Named(this.id, this.name);
  final String id;
  final String name;
}

BoatHit hit(String robotId, String robot, String org, String loc) => (
      robot: _Named(robotId, robot),
      org: _Named('org-$org', org),
      loc: _Named('loc-$loc', loc),
    );

void main() {
  final all = [
    hit('r1', 'Checkmate', 'erh', 'Newport'),
    hit('r2', 'Eliot16', 'erh', 'Newport'),
    hit('r3', 'Andiamo', 'friends', 'Block Island'),
    hit('r1', 'Checkmate', 'shared-org', 'Newport'), // same boat, second org
  ];

  test('matches the machine name, case-insensitively', () {
    final hits = filterBoatHits(all, 'checkm');
    expect(hits, hasLength(1));
    expect(hits.single.robot.name, 'Checkmate');
  });

  test('matches the location name too', () {
    expect(filterBoatHits(all, 'block island').single.robot.name, 'Andiamo');
  });

  test('does NOT match the org name — that returned every boat at once', () {
    expect(filterBoatHits(all, 'friends'), isEmpty);
  });

  test('dedupes a boat reachable through two orgs, keeping the first', () {
    final hits = filterBoatHits(all, 'newport');
    expect([for (final h in hits) h.robot.id], ['r1', 'r2']);
    expect(hits.first.org.name, 'erh'); // first membership path wins
  });

  test('sorts by machine name and returns nothing for a blank query', () {
    final hits = filterBoatHits(all, 'a'); // Andiamo + Checkmate, by name
    final names = [for (final h in hits) h.robot.name as String];
    expect(names, List.of(names)..sort());
    expect(filterBoatHits(all, '   '), isEmpty);
  });
}
