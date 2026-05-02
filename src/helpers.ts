function tankSort(names) {
  const compareFunction = function (a, b) {
    const score = function (n) {
      let num = 0;

      if (n.indexOf("freshwater") >= 0) {
        num -= 10000;
      }

      if (n.indexOf("fwd") >= 0) {
        num -= 1000;
      }

      if (n.indexOf("mid") >= 0) {
        num -= 500;
      }

      if (n.indexOf("aft") >= 0) {
        num -= 250;
      }

      if (n.indexOf("main") >= 0) {
        num += 50;
      }

      return num;
    };

    const sa = score(a);
    const sb = score(b);
    if (sa == sb) {
      return a.localeCompare(b, undefined, { numeric: true });
    }
    return sa - sb;
  };
  names.sort(compareFunction);
  return names;
}

export { tankSort };
