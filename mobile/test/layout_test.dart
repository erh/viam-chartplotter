import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:shared_preferences/shared_preferences.dart';
import 'package:viam_chartplotter_mobile/boat_state.dart';
import 'package:viam_chartplotter_mobile/data_drawer.dart';
import 'package:viam_chartplotter_mobile/map_screen.dart';
import 'package:viam_chartplotter_mobile/settings.dart';
import 'package:viam_chartplotter_mobile/viam_connection.dart';

// L6 — one adaptive layout: phone keeps the chart full-screen with the data
// drawer behind the dashboard button; ≥840 px promotes the drawer's content
// to a persistent side panel.

Future<void> pumpMap(WidgetTester tester, Size size) async {
  SharedPreferences.setMockInitialValues({});
  Settings.setForTesting(await SharedPreferences.getInstance());
  final state = BoatState();
  await tester.binding.setSurfaceSize(size);
  tester.view.physicalSize = size;
  tester.view.devicePixelRatio = 1.0;
  addTearDown(tester.view.reset);
  await tester.pumpWidget(MaterialApp(
    home: MapScreen(state: state, connection: ViamConnection(state)),
  ));
  await tester.pump();
}

void main() {
  testWidgets('phone: no persistent panel, dashboard button opens drawer',
      (tester) async {
    await pumpMap(tester, const Size(400, 800));
    expect(find.byType(DataPanel), findsNothing);
    expect(find.byTooltip('Boat data'), findsOneWidget);
  });

  testWidgets('tablet landscape: persistent panel, no dashboard button',
      (tester) async {
    await pumpMap(tester, const Size(1024, 768));
    expect(find.byType(DataPanel), findsOneWidget);
    expect(find.byTooltip('Boat data'), findsNothing);
  });
}
