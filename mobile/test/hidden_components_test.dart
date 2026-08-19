import 'package:flutter_test/flutter_test.dart';
import 'package:viam_chartplotter_mobile/viam_connection.dart';

void main() {
  test('components with truthy chartplotter-hide are hidden', () {
    const cfg = '''
    {"components": [
      {"name": "engine-cam", "type": "camera",
       "attributes": {"chartplotter-hide": true}},
      {"name": "salon-cam", "type": "camera", "attributes": {}},
      {"name": "old-depth", "type": "sensor",
       "attributes": {"chartplotter-hide": "true"}},
      {"name": "num-hide", "type": "sensor",
       "attributes": {"chartplotter-hide": 1}},
      {"name": "shown-zero", "type": "sensor",
       "attributes": {"chartplotter-hide": 0}},
      {"name": "shown-false", "type": "sensor",
       "attributes": {"chartplotter-hide": false}}
    ]}''';
    expect(hiddenComponentNames(cfg),
        {'engine-cam', 'old-depth', 'num-hide'});
  });

  test('garbage, empty, and shapeless configs yield an empty set', () {
    expect(hiddenComponentNames(''), isEmpty);
    expect(hiddenComponentNames('not json'), isEmpty);
    expect(hiddenComponentNames('{"components": "nope"}'), isEmpty);
    expect(hiddenComponentNames('{"services": []}'), isEmpty);
    expect(hiddenComponentNames('{"components": [{"attributes": null}]}'),
        isEmpty);
  });
}
