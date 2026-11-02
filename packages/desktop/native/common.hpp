#pragma once
#include <cstdio>
#include <cstdlib>
#include <cstdint>
#include <cstring>
#include <string>
#include <stdexcept>
#include <limits>
inline uint64_t number(const char* value) {
    if (!value || !*value) throw std::runtime_error("invalid identifier");
    for (auto p = value; *p; ++p) if (*p < '0' || *p > '9') throw std::runtime_error("invalid identifier");
    char* end = nullptr; errno = 0; auto n = std::strtoull(value, &end, 10);
    if (errno || *end || !n) throw std::runtime_error("invalid identifier");
    return n;
}
inline std::string quote(const std::string& value) {
    std::string out = "\"";
    for (unsigned char c : value) {
        if (c == '"' || c == '\\') { out += '\\'; out += char(c); }
        else if (c < 32) { char escaped[7]; std::snprintf(escaped, sizeof escaped, "\\u%04x", c); out += escaped; }
        else out += char(c);
    }
    return out + '"';
}
inline int failure(const std::exception& error) { std::fprintf(stderr, "dilla audio: %s\n", error.what()); return 1; }
