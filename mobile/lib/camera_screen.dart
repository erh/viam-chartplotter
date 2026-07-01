import 'dart:async';
import 'dart:typed_data';

import 'package:flutter/material.dart';
import 'package:viam_sdk/viam_sdk.dart';

/// Shows the boat's cameras, refreshing ~1 Hz. Polling only runs while this
/// screen is open (opened from the map's camera button), so cameras don't
/// consume bandwidth in the background.
class CameraScreen extends StatefulWidget {
  const CameraScreen({super.key, required this.robot, required this.names});
  final RobotClient robot;
  final List<String> names;

  @override
  State<CameraScreen> createState() => _CameraScreenState();
}

class _CameraScreenState extends State<CameraScreen> {
  final Map<String, Uint8List> _frames = {};
  final Map<String, String> _errors = {};
  Timer? _timer;
  bool _busy = false;

  @override
  void initState() {
    super.initState();
    _tick();
    _timer =
        Timer.periodic(const Duration(seconds: 1), (_) => _tick());
  }

  Future<void> _tick() async {
    if (_busy || !mounted) return;
    _busy = true;
    for (final name in widget.names) {
      try {
        final res = await Camera.fromRobot(widget.robot, name).getImages();
        if (res.images.isNotEmpty) {
          _frames[name] = Uint8List.fromList(res.images.first.image.raw);
          _errors.remove(name);
        }
      } catch (e) {
        _errors[name] = '$e';
      }
    }
    if (mounted) setState(() {});
    _busy = false;
  }

  @override
  void dispose() {
    _timer?.cancel();
    super.dispose();
  }

  @override
  Widget build(BuildContext context) {
    return Scaffold(
      appBar: AppBar(title: const Text('Cameras')),
      body: ListView(
        padding: const EdgeInsets.all(8),
        children: [
          for (final name in widget.names)
            Padding(
              padding: const EdgeInsets.symmetric(vertical: 8),
              child: Column(
                crossAxisAlignment: CrossAxisAlignment.start,
                children: [
                  Text(name,
                      style: const TextStyle(fontWeight: FontWeight.w600)),
                  const SizedBox(height: 4),
                  AspectRatio(
                    aspectRatio: 4 / 3,
                    child: Container(
                      color: Colors.black,
                      alignment: Alignment.center,
                      child: _frameFor(name),
                    ),
                  ),
                ],
              ),
            ),
        ],
      ),
    );
  }

  Widget _frameFor(String name) {
    final bytes = _frames[name];
    if (bytes != null) {
      return Image.memory(bytes, gaplessPlayback: true, fit: BoxFit.contain);
    }
    final err = _errors[name];
    if (err != null) {
      return Padding(
        padding: const EdgeInsets.all(12),
        child: Text(err,
            textAlign: TextAlign.center,
            style: const TextStyle(color: Colors.redAccent, fontSize: 12)),
      );
    }
    return const CircularProgressIndicator();
  }
}
