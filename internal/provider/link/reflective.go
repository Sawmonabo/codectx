package link

// reflective holds the external methods of the reflection APIs that run a
// method, constructor or function chosen at run time, by externalMethod key.
// A call of one is published to that method and is also an open site of
// reason model.ReasonReflective, since what it runs is not known:
//   - Java: java.lang.reflect.Method.invoke and Constructor.newInstance,
//     java.lang.Class.newInstance, and java.lang.invoke.MethodHandle's
//     invoke, invokeExact and invokeWithArguments (the Java SE API; the set
//     call-graph builders model as reflective calls);
//   - Go: reflect.Value's Call and CallSlice, which call the function or
//     method the value holds (package reflect).
//
// A computed callee (`obj[name]()`, `getattr(obj, name)()`) is an open site
// of reason computed already, and a call through a function value of any
// origin (a C `dlsym` result) one of value_untracked.
var reflective = map[string]bool{
	externalMethod("java", "java.lang.reflect.Method", "invoke"):                   true,
	externalMethod("java", "java.lang.reflect.Constructor", "newInstance"):         true,
	externalMethod("java", "java.lang.Class", "newInstance"):                       true,
	externalMethod("java", "java.lang.invoke.MethodHandle", "invoke"):              true,
	externalMethod("java", "java.lang.invoke.MethodHandle", "invokeExact"):         true,
	externalMethod("java", "java.lang.invoke.MethodHandle", "invokeWithArguments"): true,
	externalMethod("go", "reflect.Value", "Call"):                                  true,
	externalMethod("go", "reflect.Value", "CallSlice"):                             true,
}
