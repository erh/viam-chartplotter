import 'package:flutter/material.dart';

import 'boat_state.dart';
import 'map_screen.dart';
import 'viam_connection.dart';

void main() {
  runApp(const ChartplotterApp());
}

class ChartplotterApp extends StatefulWidget {
  const ChartplotterApp({super.key});

  @override
  State<ChartplotterApp> createState() => _ChartplotterAppState();
}

class _ChartplotterAppState extends State<ChartplotterApp> {
  final BoatState _state = BoatState();
  late final ViamConnection _conn = ViamConnection(_state);

  @override
  void initState() {
    super.initState();
    _conn.start();
  }

  @override
  void dispose() {
    _conn.dispose();
    _state.dispose();
    super.dispose();
  }

  @override
  Widget build(BuildContext context) {
    return MaterialApp(
      title: 'Viam Chartplotter',
      debugShowCheckedModeBanner: false,
      theme: ThemeData.dark(useMaterial3: true),
      home: MapScreen(state: _state),
    );
  }
}
