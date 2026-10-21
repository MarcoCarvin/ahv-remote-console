{
  "targets": [
    {
      "target_name": "keyhook",
      "sources": [ "src/keyhook.cc" ],
      "include_dirs": [
        "<!@(node -p \"require('node-addon-api').include_dir\")"
      ],
      "defines": [ "NAPI_VERSION=9" ],
      "cflags_cc": [ "-fexceptions", "-std=c++17" ],
      "conditions": [
        [ "OS=='win'", {
          "sources": [ "src/win.cc" ]
        } ],
        [ "OS=='mac'", {
          "sources": [ "src/mac.mm" ],
          "xcode_settings": {
            "CLANG_CXX_LIBRARY": "libc++",
            "CLANG_CXX_LANGUAGE_STANDARD": "c++17",
            "MACOSX_DEPLOYMENT_TARGET": "11.0",
            "GCC_ENABLE_CPP_EXCEPTIONS": "YES"
          }
        } ],
        [ "OS=='linux'", {
          "sources": [ "src/linux.cc" ]
        } ]
      ]
    }
  ]
}
