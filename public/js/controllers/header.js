scionApp
    .controller('headerCtrl', ['$scope', 'loginService', '$location', '$window',
        function ($scope, loginService, $location, $window) {

            $scope.isActive = function (viewLocation) {
                //return viewLocation === $location.path();
                //return $location.path().startsWith(viewLocation);
                expr = viewLocation.replace('/', '^\/').replace('*', '[a-z0-9-]+');
                res = RegExp(expr).test($location.path());
                alert (expr + '\n' + $location.path() + '\n' + res);
                return res;
            };

            $scope.logout = function () {
                loginService.logout().then(
                    function (response) {
                        $window.location.href = '/';
                    },
                    function (error) {
                        console.log(response);
                    });
            };
        }
    ]);
