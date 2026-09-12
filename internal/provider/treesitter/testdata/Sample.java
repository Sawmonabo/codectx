package com.example;

import java.util.List;
import org.junit.Test;

/** A server class; héllo → 日本. */
public class Sample {
    private static final String GREETING = "héllo → 日本";
    private int port = 8080;

    /** Nested interface. */
    public interface Handler {
        void serve(String name);
    }

    public void start(String name) {
        Runnable inner = new Runnable() {
            public void run() { helper(name); }
        };
        inner.run();
        List<String> names = List.of(name);
    }

    static void helper(String name) {}

    @Test
    public void startsTheServer() { new Sample().start("x"); }
}
