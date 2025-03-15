

function tankSort(names) {

  var compareFunction = function(a, b) {
  
    var score = function(n) {
      var num = 0;
      
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
    }

    var sa = score(a);
    var sb = score(b);
    if (sa == sb) {
      if (sa < sb) {
        return -1;
      }
      if (sa > sb) {
        return 1;
      }
      return 0;
    }
    return sa - sb;
  }
  names.sort(compareFunction);
  return names;
}

export { tankSort }
