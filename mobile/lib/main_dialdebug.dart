// Debug entrypoint: replay the machine picker's exact connect sequence with
// no UI interaction, so the dial can be tested headlessly in debug AND
// release builds. Not part of the product.
//
//   flutter run -t lib/main_dialdebug.dart -d macos --release \
//     --dart-define=DBG_ORG_KEY_ID=... --dart-define=DBG_ORG_KEY=... \
//     --dart-define=DBG_ROBOT_ID=... --dart-define=DBG_ORG_ID=...
import 'package:flutter/material.dart';
import 'package:viam_sdk/viam_sdk.dart';

const _orgKeyId = String.fromEnvironment('DBG_ORG_KEY_ID');
const _orgKey = String.fromEnvironment('DBG_ORG_KEY');
const _robotId = String.fromEnvironment('DBG_ROBOT_ID');
const _orgId = String.fromEnvironment('DBG_ORG_ID');

void main() {
  WidgetsFlutterBinding.ensureInitialized();
  runApp(const MaterialApp(home: Scaffold(body: Center(child: Text('dial debug — watch stdout')))));
  _run();
}

void _log(String m) {
  // ignore: avoid_print
  print('DIALDEBUG: $m');
}

Future<void> _run() async {
  final sw = Stopwatch()..start();
  try {
    _log('building Viam handle with org api key…');
    final viam = await Viam.withApiKey(_orgKeyId, _orgKey);
    _log('app channel up at ${sw.elapsed}; listing parts…');
    final parts = await viam.appClient.listRobotParts(_robotId);
    final part = parts.firstWhere((p) => p.mainPart);
    _log('main part ${part.fqdn} at ${sw.elapsed}; minting machine key…');
    final resp = await viam.appClient.createKey(
      [
        ViamAuthorization(
          authorizationId: AuthorizationId.robotOwner,
          resourceType: ResourceType.robot,
          resourceId: _robotId,
          organizationId: _orgId,
          identityType: IdentityType.apiKey,
        ),
      ],
      'dialdebug-temp',
    );
    _log('minted key ${resp.id} at ${sw.elapsed}; dialing (mdns off)…');
    final options = RobotClientOptions.withApiKey(resp.id, resp.key)
      ..dialOptions.attemptMdns = false;
    final robot = await RobotClient.atAddress(part.fqdn, options);
    _log('CONNECTED at ${sw.elapsed}; ${robot.resourceNames.length} resources');
    await robot.close();
    await viam.appClient.deleteKey(resp.id);
    _log('temp key deleted; DONE');
  } catch (e, st) {
    _log('FAILED at ${sw.elapsed}: $e\n$st');
  }
}
