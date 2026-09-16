// A JavaScript project with no compiler configuration: package.json is the
// only manifest, which is what the plain-JavaScript half of a repository
// looks like.
const salutación = "héllo → 日本";

class Répertoire {
  constructor(nombre) {
    this.nombre = nombre;
  }
  greet() {
    return salutación + " " + this.nombre;
  }
}

function build(nombre) {
  return new Répertoire(nombre).greet();
}

module.exports = { build, Répertoire, salutación };
