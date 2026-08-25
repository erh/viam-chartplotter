import 'package:flutter_test/flutter_test.dart';
import 'package:viam_chartplotter_mobile/screens/machine_picker_screen.dart';

// Machine liveness in the picker. The whole point is that it comes off the
// `listRobots` record — `Robot.onlineState` / `Robot.secondsSinceOnline` —
// so nothing here dials a machine, and these fakes stand in for the proto.

/// Stands in for the generated `OnlineState` enum value, which is read by
/// name.
class _State {
  _State(this.name);
  final String name;
}

class _Robot {
  _Robot(this.name, String state, {this.secondsSinceOnline})
      : onlineState = _State(state);
  final String name;
  final _State onlineState;
  final int? secondsSinceOnline;
}

/// A location or org row: no liveness fields at all.
class _Folder {
  _Folder(this.name);
  final String name;
}

void main() {
  group('machineStatusFromName', () {
    test('maps the proto states', () {
      expect(machineStatusFromName('ONLINE_STATE_ONLINE'), MachineStatus.online);
      expect(
          machineStatusFromName('ONLINE_STATE_OFFLINE'), MachineStatus.offline);
      expect(machineStatusFromName('ONLINE_STATE_AWAITING_SETUP'),
          MachineStatus.awaitingSetup);
    });

    test('unspecified, null and unrecognised states read as unknown', () {
      expect(machineStatusFromName('ONLINE_STATE_UNSPECIFIED'),
          MachineStatus.unknown);
      expect(machineStatusFromName(null), MachineStatus.unknown);
      expect(machineStatusFromName('ONLINE_STATE_FUTURE'),
          MachineStatus.unknown);
    });
  });

  group('machineStatusOf', () {
    test('reads the record the API returned', () {
      expect(machineStatusOf(_Robot('Checkmate', 'ONLINE_STATE_ONLINE')),
          MachineStatus.online);
      expect(machineStatusOf(_Robot('Andiamo', 'ONLINE_STATE_OFFLINE')),
          MachineStatus.offline);
    });

    test('an item with no liveness fields reads as unknown, not a crash', () {
      expect(machineStatusOf(_Folder('Newport')), MachineStatus.unknown);
      expect(machineSecondsSinceOnline(_Folder('Newport')), isNull);
    });
  });

  group('offlineForLabel', () {
    test('coarsens seconds to one unit', () {
      expect(offlineForLabel(45), '45s');
      expect(offlineForLabel(90), '1m');
      expect(offlineForLabel(3599), '59m');
      expect(offlineForLabel(3600), '1h');
      expect(offlineForLabel(86399), '23h');
      expect(offlineForLabel(86400), '1d');
      expect(offlineForLabel(86400 * 9), '9d');
    });

    test('nothing to say when the API gave no usable value', () {
      expect(offlineForLabel(null), isNull);
      expect(offlineForLabel(0), isNull);
      expect(offlineForLabel(-5), isNull);
    });
  });

  group('machineStatusLabel', () {
    test('an offline machine says how long it has been gone', () {
      expect(machineStatusLabel(MachineStatus.offline, 7200),
          'Offline · last seen 2h ago');
    });

    test('an offline machine with no duration still says offline', () {
      expect(machineStatusLabel(MachineStatus.offline, null), 'Offline');
      expect(machineStatusLabel(MachineStatus.offline, 0), 'Offline');
    });

    test('online and awaiting-setup label themselves', () {
      expect(machineStatusLabel(MachineStatus.online, 0), 'Online');
      expect(machineStatusLabel(MachineStatus.awaitingSetup, null),
          'Awaiting setup');
    });

    test('unknown gets no label, so the row renders as it always did', () {
      expect(machineStatusLabel(MachineStatus.unknown, 999), isNull);
    });
  });

  test('a machine row still filters by name alongside its status', () {
    final robots = [
      _Robot('Checkmate', 'ONLINE_STATE_ONLINE'),
      _Robot('Andiamo', 'ONLINE_STATE_OFFLINE'),
    ];
    final shown = filterByName(robots, 'andi');
    expect(shown, hasLength(1));
    expect(machineStatusOf(shown.single), MachineStatus.offline);
  });
}
