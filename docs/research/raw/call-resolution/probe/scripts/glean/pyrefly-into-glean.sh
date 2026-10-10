# Runs inside the Glean demo container. $1 = schema dir, $2 = JSON dir, $3 = db name
set -u
S="$1"; J="$2"; N="$3"
G="glean --db-root /glean-db --schema dir:$S"
f() { grep -v -E '^[IW][0-9]{4} ' ; }
mkdir -p /glean-db
echo "## create"; $G create --repo $N/0 2>&1 | f
echo "## write"; $G write --repo $N/0 $J/*.json 2>&1 | f
echo "## derive"
for p in python.ImportStatementByAsName python.DeclarationsByFile python.SNameToName python.ModuleBySName python.ClassBySName python.FunctionBySName python.VariableBySName python.DeclarationDefinition python.DefinitionsByFile python.ImportStarsByFile python.BaseClassToDerived python.Contains python.ContainedByTopLevelDeclaration python.DeclarationUses; do
  $G derive --repo $N/0 $p 2>&1 | f | grep -i -E 'error|unknown|fail' | sed "s#^#derive $p: #"
done
echo "## finish"; $G finish --repo $N/0 2>&1 | f
q() { echo; echo "## query: $1"; $G query --repo $N/0 --expand "$1" 2>&1 | f | head -${2:-60}; }
q 'python.CalleeToCaller _'
q 'python.MethodOverrides _'
q 'python.MethodOverriden _'
q 'python.DirectXRefsByFile _' 300
for n in animals.Animal.sound animals.Dog.sound animals.Cat.sound zoo.Base.sound zoo.Dog.sound zoo.Animal.sound; do
  q "python.DeclarationUses { declaration = { func = { name = \"$n\" } } }"
  q "python.DeclarationWithName { name = \"$n\" }"
done
for n in animals.Animal.sound animals.Dog.sound zoo.Base.sound zoo.Dog.sound; do q "codemarkup.EntityUses { target = { python = { decl = { func = { name = \"$n\" } } } } }"; done
echo; echo "## image schema DeclarationUses / DirectXRefsByFile definitions"; grep -n -A12 -E "^predicate (DeclarationUses|DirectXRefsByFile)" $S/python.angle
