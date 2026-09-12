#include <string>
#include <vector>
using std::string;

// Non-ASCII: héllo → 日本
static const string greeting = "héllo → 日本";

namespace app {

/// A server class.
class Server {
public:
    explicit Server(string name) : name_(std::move(name)) {}
    int start();
    struct Config { int port = 8080; };
private:
    string name_;
};

int helper(const string &name) { return static_cast<int>(name.size()); }

int Server::start() {
    auto inner = [this]() { return helper(name_); };
    std::vector<int> v{inner()};
    return v.front();
}

}  // namespace app
